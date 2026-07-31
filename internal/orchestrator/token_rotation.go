package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/folsomintel/fuse/internal/secrets"
)

// RotateToken generates new TLS credentials and an auth token for a
// running VM, uploads them to the guest filesystem, and persists the
// new encrypted token.
//
// KNOWN GAP: fused reads --auth-token-file exactly once at process start
// (fused/main.go) and has no credential poller, so a live fused keeps
// serving the OLD token until it is restarted. Rotation therefore updates the
// orchestrator's copy and the guest's files but does not yet take effect in the
// running guest; it needs a StartAgent call (which restarts fused) the way
// ForkEnvironment does.
//
// Rotation is a server-side operation: the orchestrator updates its
// own encrypted copy and the guest agent's credential files (paths owned
// by the agent profile). New inbound connections use the rotated
// credentials; existing connections retain the old cert until they reconnect.
func (fm *FleetManager) RotateToken(ctx context.Context, vmID string) error {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	if !ok {
		fm.mu.RUnlock()
		return fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	env := v.env
	state := v.state
	fm.mu.RUnlock()

	if state != VMStateRunning {
		return fmt.Errorf("vm %s in state %s: token rotation requires running", vmID, state)
	}
	if env == nil {
		return fmt.Errorf("vm %s has no active environment handle", vmID)
	}
	if len(fm.tokenEncryptionKey) != 32 {
		return fmt.Errorf("token rotation requires a 32-byte encryption key (TOKEN_ENCRYPTION_KEY)")
	}

	// Generate fresh credentials.
	creds, err := secrets.GenerateVMCredentials(vmID)
	if err != nil {
		return fmt.Errorf("generate credentials: %w", err)
	}

	// Upload the agent's credential files (cert, key, auth token) to the
	// guest. Paths are owned by the agent profile (fusedCredentialFiles).
	if err := uploadFiles(ctx, env, fusedCredentialFiles(creds)); err != nil {
		return fmt.Errorf("upload credentials: %w", err)
	}
	setTokenIfSupported(env, creds)

	// Encrypt and persist the new token.
	encrypted, err := secrets.EncryptToken(creds.AuthToken, fm.tokenEncryptionKey)
	if err != nil {
		return fmt.Errorf("encrypt token: %w", err)
	}

	fm.mu.Lock()
	if v, ok := fm.vms[vmID]; ok {
		v.authTokenEncrypted = encrypted
		v.updatedAt = time.Now()
	}
	fm.mu.Unlock()

	fm.persistVMBackground(vmID)
	fm.appendEvent(ctx, "vm", vmID, "vm.token_rotated", nil)
	fm.logger.Info("vm token rotated", "vm", vmID)

	return nil
}
