package orchestrator

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// Exec runs argv inside a running VM's guest and reports the result.
//
// A non-zero guest exit code is not an error: it is carried in
// ExecResult.ExitCode with stdout and stderr intact, so a caller can tell
// "the command ran and failed" apart from "the command could not be run".
// The returned error is reserved for the latter — unknown VM, wrong state,
// or a transport failure reaching the host.
//
// Only Running VMs can be exec'd into. A Draining VM is deliberately refused
// even though its guest is still reachable: drain means the workload is being
// quiesced, and letting new commands in behind that would undo it.
func (fm *FleetManager) Exec(ctx context.Context, vmID string, cmd []string, opts ExecOptions) (ExecResult, error) {
	if len(cmd) == 0 {
		return ExecResult{}, fmt.Errorf("exec: empty command")
	}

	env, err := fm.guestEnvironment(ctx, vmID)
	if err != nil {
		return ExecResult{}, err
	}

	if opts.Timeout <= 0 {
		opts.Timeout = DefaultExecTimeout
	}
	if opts.Timeout > MaxExecTimeout {
		opts.Timeout = MaxExecTimeout
	}

	// an exec attempt is activity whether or not the command succeeds: the
	// caller is clearly still using the environment.
	fm.touchActivity(vmID)

	res, err := env.Exec(ctx, cmd, opts)
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec in vm %s: %w", vmID, err)
	}
	return res, nil
}

// Attach opens a raw duplex byte stream to a process inside a running VM's
// guest. The stream carries fuse-attach/1 frames; the fleet does not interpret
// them, it only hands the stream back to the caller to relay.
//
// The caller owns the returned stream and must Close it.
func (fm *FleetManager) Attach(ctx context.Context, vmID string, spec AttachSpec) (io.ReadWriteCloser, error) {
	env, err := fm.guestEnvironment(ctx, vmID)
	if err != nil {
		return nil, err
	}

	a, ok := env.(Attacher)
	if !ok {
		return nil, fmt.Errorf("%w: vm %s", ErrAttachUnsupported, vmID)
	}

	stream, err := a.Attach(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("attach to vm %s: %w", vmID, err)
	}

	// an open attach session counts as activity for as long as it is open, not
	// just at the moment it opens: an interactive shell can sit quiet for
	// hours and is still in use. the session is released when the caller
	// closes the stream, which also restarts the idle window.
	fm.openAttachSession(vmID)
	return &activityStream{ReadWriteCloser: stream, release: func() { fm.closeAttachSession(vmID) }}, nil
}

// activityStream releases a VM's attach session exactly once, when the caller
// closes the stream it owns.
type activityStream struct {
	io.ReadWriteCloser
	once    sync.Once
	release func()
}

func (s *activityStream) Close() error {
	err := s.ReadWriteCloser.Close()
	s.once.Do(s.release)
	return err
}

// touchActivity records control-plane activity against a VM, restarting its
// idle window. Unknown VM ids are ignored.
func (fm *FleetManager) touchActivity(vmID string) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if v, ok := fm.vms[vmID]; ok {
		v.lastActivityAt = time.Now()
	}
}

// openAttachSession marks a VM as holding one more live attach session.
func (fm *FleetManager) openAttachSession(vmID string) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if v, ok := fm.vms[vmID]; ok {
		v.attachSessions++
		v.lastActivityAt = time.Now()
	}
}

// closeAttachSession releases one live attach session and restarts the idle
// window from the moment the session ended.
func (fm *FleetManager) closeAttachSession(vmID string) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if v, ok := fm.vms[vmID]; ok {
		if v.attachSessions > 0 {
			v.attachSessions--
		}
		v.lastActivityAt = time.Now()
	}
}

// guestEnvironment resolves a VM id to a live Environment handle, enforcing
// the precondition every guest-touching operation shares: the VM must be
// tracked and Running.
//
// The cached handle (v.env) is the fast path; it is set at provision time and
// rehydrated by recoverState. If it is missing we fall back to asking the VM's
// host provider for a fresh handle, the same way snapshotEnvironment does.
func (fm *FleetManager) guestEnvironment(ctx context.Context, vmID string) (Environment, error) {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	if !ok {
		fm.mu.RUnlock()
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	if v.state != VMStateRunning {
		current := v.state
		fm.mu.RUnlock()
		return nil, fmt.Errorf("%w: vm %s is in state %s", ErrVMNotRunning, vmID, current)
	}
	if v.env != nil {
		env := v.env
		fm.mu.RUnlock()
		return env, nil
	}

	provider := fm.provider
	if v.hostID != "" {
		if hostProvider, ok := fm.providerForHost(v.hostID); ok {
			provider = hostProvider
		}
	}
	fm.mu.RUnlock()

	if provider == nil {
		return nil, fmt.Errorf("no provider available for vm %s", vmID)
	}
	env, err := provider.Get(ctx, vmID)
	if err != nil {
		return nil, fmt.Errorf("get environment %s: %w", vmID, err)
	}
	return env, nil
}
