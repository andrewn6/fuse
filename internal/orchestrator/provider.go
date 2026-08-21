// Package orchestrator defines the interface for sandbox providers and
// implements the boot flow that provisions or restores environments.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/folsomintel/fuse/internal/secrets"
)

// Provider manages sandboxed environments.
type Provider interface {
	// Create provisions a new sandbox.
	Create(ctx context.Context, spec Spec) (Environment, error)

	// Get returns a handle to an existing sandbox by name.
	Get(ctx context.Context, name string) (Environment, error)

	// Destroy tears down a sandbox.
	Destroy(ctx context.Context, name string) error

	// List returns all sandboxes matching the given prefix.
	List(ctx context.Context, prefix string) ([]Environment, error)

	// Close releases provider resources.
	Close() error
}

// Spec describes the resources needed for a sandbox.
type Spec struct {
	Name      string // e.g. "fuse-{task-id}"
	CPUs      int
	RamMB     int
	StorageGB int
	Region    string

	// Arch restricts scheduling to hosts of this CPU architecture, in
	// GOARCH vocabulary ("amd64", "arm64"). Empty means any host: images
	// are resolved host-side, so a spec that doesn't care about arch keeps
	// the pre-arch behavior. A host with no reported arch counts as amd64.
	Arch string

	// GPUs is the count of GPU devices requested: whole devices, or MIG
	// instances when GPUProfile is set. Zero means no GPU.
	GPUs int32

	// GPUKind identifies the requested GPU model (e.g. "a100"). Empty when
	// GPUs is 0.
	GPUKind string

	// GPUProfile requests fractional GPU allocation: a MIG profile in
	// mig-parted vocabulary (e.g. "1g.10gb"), passed through as an mdev
	// device by the qemu host agent (decision D5). Empty means whole-device
	// allocation.
	GPUProfile string

	// GPUUUIDs are the concrete device uuids this VM is bound to. Populated
	// by allocation on hosts that report per-device inventory
	// (Capacity.GPUDevices non-empty) and persisted per-VM so the binding
	// survives an orchestrator restart. Empty on legacy homogeneous hosts
	// that only track a scalar GPU count.
	GPUUUIDs []string `json:"gpu_uuids,omitempty"`

	// MIGInstanceUUIDs are the concrete MIG instance uuids this VM is bound
	// to. Populated by allocation on hosts that report per-instance MIG
	// inventory (Capacity.MIGInstances non-empty) and persisted per-VM so
	// the binding survives an orchestrator restart. Empty when the host
	// only tracks MIG allocation by profile count (the MIGProfiles path).
	// Mirrors GPUUUIDs for whole devices (issue #41).
	MIGInstanceUUIDs []string `json:"mig_instance_uuids,omitempty"`

	// HostID pins this sandbox to an exact host id. Unlike GPUUUIDs and
	// MIGInstanceUUIDs, which allocation fills in, this is caller-settable:
	// it comes from the Fusefile's placement.host. A pin is a hard gate, not
	// an override, so the pinned host still has to be active, run the right
	// backend, and fit the spec.
	HostID string

	// Labels are placement label selectors: every pair must match the target
	// host's declared labels. Caller-settable, from placement.labels.
	Labels map[string]string

	// MaxRuntime overrides FleetConfig.TaskStuckTimeout for this task.
	// Zero means "use the fleet default". This is a leak-detection ceiling,
	// not a liveness check — set it higher than any plausible healthy runtime.
	MaxRuntime time.Duration

	// IdleTimeout destroys this VM once it has gone this long with no exec
	// and no attach session. Zero means "use FleetConfig.DefaultIdleTimeout"
	// (itself zero by default, i.e. no idle expiry). Unlike MaxRuntime this
	// is a liveness check, but only over control-plane activity: in-guest CPU
	// and network traffic are not observed.
	IdleTimeout time.Duration

	// Image names a base rootfs for the provider to boot from, resolved by
	// the provider (e.g. the firecracker host agent looks it up in its own
	// named-rootfs directory). Empty means the provider's default base.
	Image string

	// SeedSnapshotID boots this VM from a persisted snapshot artifact instead
	// of Image. Unlike a fork there is no live source VM: the artifact is
	// resolved out of the host agent's snapshot store. It is host-local, which
	// is why SeedSnapshotID always travels with PinnedHostID. Empty means boot
	// from Image. Mutually exclusive with Image.
	SeedSnapshotID string

	// PinnedHostID restricts scheduling to a single host. Set when the VM must
	// land where its host-local seed artifact already is. Empty means schedule
	// across the fleet as usual.
	PinnedHostID string
}

// ExposeSpec requests that a guest port be published as a reachable
// endpoint by the provider during StartAgent.
type ExposeSpec struct {
	Port int
	As   string
}

// Endpoint is a published network endpoint for an environment (e.g. an
// ingress port exposed via the Fusefile's `expose` list).
type Endpoint struct {
	As   string // caller-chosen label, e.g. "http"
	URL  string // reachable address, e.g. "http://203.0.113.5:41231"
	Port int    // the guest-side port this endpoint publishes
}

// Environment is a running sandbox.
type Environment interface {
	// Name returns the sandbox identifier.
	Name() string

	// URL returns the sandbox's reachable address for the guest agent.
	URL() string

	// Exec runs argv inside the sandbox and reports the result. A non-zero
	// guest exit code is carried in ExecResult.ExitCode, not returned as an
	// error; err is reserved for transport and provider failures, so callers
	// must check both.
	Exec(ctx context.Context, cmd []string, opts ExecOptions) (ExecResult, error)

	// ExecStream runs a command with stdout/stderr wired to writers.
	ExecStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error

	// Upload writes data to a file path inside the sandbox.
	Upload(ctx context.Context, data []byte, path string) error

	// StartAgent launches the configured guest agent inside the sandbox.
	StartAgent(ctx context.Context, spec AgentSpec) error

	// Token returns the per-sandbox auth token that callers must include
	// when reaching URL(). Empty string for providers whose URL
	// self-authenticates.
	Token() string
}

// Exec timeout bounds. Guest commands are bounded so a hung command cannot
// pin an SSH session on the host agent forever. MaxExecTimeout matches the
// host agent's own per-exec ceiling; asking for more would let the caller's
// deadline outlive the guest command it is waiting on.
const (
	DefaultExecTimeout = 60 * time.Second
	MaxExecTimeout     = 600 * time.Second
)

// ExecOptions tunes a single Exec call.
type ExecOptions struct {
	// Timeout bounds how long the guest command may run. Zero means
	// DefaultExecTimeout. Values above MaxExecTimeout are clamped.
	Timeout time.Duration
}

// ExecResult is the outcome of running a command inside a sandbox. Stdout and
// Stderr are kept separate and byte-exact; ExitCode is the guest command's
// own status, which is meaningful even when it is non-zero.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// AttachSpec describes an interactive attach to a sandbox.
type AttachSpec struct {
	// Cmd is the argv to run. Empty means the guest's login shell.
	Cmd []string

	// TTY requests a pty for the process. Rows and Cols seed its initial
	// window size; further resizes arrive as frames on the stream itself.
	TTY  bool
	Rows uint16
	Cols uint16
}

// Attacher is implemented by environments that can open a raw duplex byte
// stream to a process inside the sandbox. The stream carries the fuse-attach/1
// frame protocol (see docs/attach.md); nothing between the caller and the
// guest interprets those frames, so the orchestrator relays them verbatim.
//
// Environments with no real guest (e.g. in-memory stubs) omit this method;
// callers type-assert and report ErrAttachUnsupported when it is absent.
type Attacher interface {
	Attach(ctx context.Context, spec AttachSpec) (io.ReadWriteCloser, error)
}

// TokenSetter is implemented by environments whose per-sandbox auth
// token can be updated after construction (e.g. set during Boot once
// credentials are generated, or refreshed by token rotation). Boot and
// RotateToken type-assert to this interface so the Environment surface
// stays read-only for callers.
type TokenSetter interface {
	SetToken(token string)
}

// SnapshotCapable is implemented by environments that support
// checkpoint/restore. Providers that cannot snapshot (e.g. some
// container-based providers) simply omit these methods; callers must
// type-assert to SnapshotCapable before invoking them.
type SnapshotCapable interface {
	Checkpoint(ctx context.Context, comment string) (string, error)
	Restore(ctx context.Context, checkpointID string) error
	ListCheckpoints(ctx context.Context) ([]Checkpoint, error)
}

// SnapshotForkable is implemented by providers that can create a brand-new
// environment seeded from an existing checkpoint of another vm. this is the
// capability a true fork needs; the firecracker provider does not implement
// it yet, so ForkEnvironment reports fork as unsupported at runtime until a
// host wire endpoint exists.
type SnapshotForkable interface {
	CreateFromCheckpoint(ctx context.Context, spec Spec, srcVMID, checkpointID string) (Environment, error)
}

// SnapshotDeleter is implemented by environments that can delete a
// previously created checkpoint by ID.
type SnapshotDeleter interface {
	DeleteCheckpoint(ctx context.Context, checkpointID string) error
}

// SnapshotDigester is an optional companion to SnapshotCapable for backends
// that hash the artifact while they write it. It exists as a separate
// interface rather than a wider Checkpoint signature because the digest is
// only ever available at creation time: the host agent computes it inline
// over the rootfs it just copied, and nothing recomputes it afterwards, so
// ListCheckpoints cannot supply it and changing Checkpoint's signature would
// churn every backend and caller for a value most of them cannot produce.
//
// An environment that does not implement this (the in-memory stub, qemu)
// simply yields no digest, which is not an error: the digest is an integrity
// check on a later transfer of those bytes, never an identity a lookup
// depends on.
type SnapshotDigester interface {
	CheckpointWithDigest(ctx context.Context, comment string) (Checkpoint, error)
}

// SnapshotKind distinguishes the two things a checkpoint can be. A disk
// snapshot is the rootfs and nothing else, so restoring it cold-boots the
// guest; a live snapshot additionally carries vCPU state and the whole guest
// memory, so restoring it resumes the guest exactly where it stopped.
//
// It is recorded rather than inferred. The two are indistinguishable once
// created unless something wrote down which one was asked for, and guessing
// wrong is not a cosmetic error: restoring a live artifact as a disk one
// silently cold-boots a guest the caller believed it had frozen.
type SnapshotKind string

const (
	// SnapshotKindDisk is the default and the fallback. Everything taken
	// before live snapshots existed is this, and anything that cannot be
	// classified reads back as this.
	SnapshotKindDisk SnapshotKind = "disk"

	// SnapshotKindLive is rootfs plus vCPU state plus the full memory image.
	SnapshotKindLive SnapshotKind = "live"
)

// LiveSnapshotCapable is an optional companion to SnapshotCapable for backends
// that can checkpoint guest memory as well as disk, in one pause window.
//
// It is a separate narrow interface rather than a wider SnapshotCapable for
// the same reason SnapshotDigester is: most backends cannot do this at all.
// Widening SnapshotCapable would be actively harmful here, not merely noisy.
// The qemu provider deliberately implements NEITHER interface (see
// internal/qemu/qemu.go and the guardrail tests in qemu_test.go), because a
// vfio-passed-through GPU cannot be checkpointed at all, and the orchestrator
// relies on that missing-method fact: the `env.(SnapshotCapable)` assertion
// failing is what stops a GPU environment from being snapshotted. Adding a
// method to SnapshotCapable that only firecracker can satisfy would force
// every backend to either implement it or drop out of the interface entirely,
// which is exactly the guardrail being leaned on.
//
// There is deliberately no RestoreLive. The host agent records the kind in the
// snapshot's own metadata and branches on it internally, so the existing
// Restore already restores a live snapshot correctly. A second restore verb
// would let a caller name a kind that disagrees with what was actually
// written, which is the one thing the recorded kind exists to prevent.
type LiveSnapshotCapable interface {
	CheckpointLive(ctx context.Context, comment string) (Checkpoint, error)
}

// CapacityProber is implemented by providers that can report the real
// hardware capacity of the host they front (CPU count, total RAM, free
// disk, GPU inventory) instead of trusting operator-declared numbers. The
// registerHost API handler type-asserts the provider to this interface at
// registration time to source capacity for any field the operator left
// unset; providers that cannot probe (e.g. a stub with no real hardware
// behind it) simply return an error.
type CapacityProber interface {
	Capacity(ctx context.Context) (HostCapacity, error)
}

// HTTPStatusError carries the HTTP status code (and trimmed body) from a
// non-2xx host-agent response so callers can branch on the code without
// importing a specific provider package. Providers wrap their raw HTTP
// errors in this type; RegisterHost uses it to tell "agent rejected the
// token" (401) apart from "agent unreachable" or any other failure when the
// register-time capacity probe (see CapacityProber) errors out.
type HTTPStatusError struct {
	Code int
	Body string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Code, e.Body)
}

// IsNotFound reports whether err is, or wraps, a provider HTTP 404. Both the
// firecracker and qemu providers surface agent responses as *HTTPStatusError,
// so cleanup paths use this to tell "the resource is already gone" apart from
// a real provider failure. A vanished resource means the work is already done,
// not that the caller should retry forever.
func IsNotFound(err error) bool {
	var statusErr *HTTPStatusError
	return errors.As(err, &statusErr) && statusErr.Code == http.StatusNotFound
}

// AgentSpec is the generic, provider-agnostic description of the guest agent
// to launch inside a sandbox. fused is expressed as one configuration of this
// spec via FusedAgentSpec (see agent_profile.go); nothing fuse-specific is
// hardcoded in the core boot path.
type AgentSpec struct {
	Files        map[string][]byte // arbitrary files to upload into the guest (path -> bytes)
	DownloadURL  string            // fetch the agent binary (e.g. GitHub releases)
	Command      string            // how to launch the daemon
	AuthToken    string            // pass-through bearer token
	Gateway      string            // pass-through gateway websocket URL
	GatewayToken string            // pass-through gateway token
	DrainCommand string            // command run inside the guest for graceful shutdown ('' => skip)
	Expose       []ExposeSpec      // guest ports to publish as reachable endpoints, if any
}

type BootOptions struct {
	StartupScript string
	GatewayURL    string
	GatewayToken  string
	Expose        []ExposeSpec

	// Healthcheck is the environment-level readiness probe, or nil when the
	// caller declared none. It is shipped into the guest as a file (see
	// FusedAgentSpec) and evaluated there; the orchestrator only reads the
	// verdict back. See health.go.
	Healthcheck *HealthcheckSpec

	// Desktop is the graphical session's geometry, or nil when the caller
	// declared none. It is shipped into the guest as a file (see
	// FusedAgentSpec) and applied there; the orchestrator never renders
	// anything. See desktop.go.
	Desktop *DesktopSpec

	// Files are caller-supplied guest files (a Fusefile's `copy` block),
	// keyed by absolute guest path. The agent profile merges them into
	// AgentSpec.Files ahead of its own entries, so they are uploaded on the
	// same pass as the manifest and credentials and therefore land before
	// StartupScript runs, and so a caller can never displace a profile file.
	// The API refuses a path under ReservedGuestDir before it reaches here.
	Files map[string][]byte

	// StartupScriptTimeout bounds StartupScript. Zero means
	// DefaultStartupScriptTimeout. The script runs synchronously inside the
	// create request, so this must stay comfortably under the orchestrator's
	// HTTP write timeout (60s by default) for the caller to receive a real
	// error instead of a truncated response.
	StartupScriptTimeout time.Duration
}

// DefaultStartupScriptTimeout bounds a boot-time startup script when
// BootOptions.StartupScriptTimeout is unset. Chosen to leave headroom under
// the default 60s HTTP write timeout so the API can answer with a classified
// error rather than having the response cut off mid-flight.
const DefaultStartupScriptTimeout = 30 * time.Second

// DefaultMaxStartupScriptTimeout is the largest bound a caller may request via
// BootOptions.StartupScriptTimeout when FleetConfig.MaxStartupScriptTimeout is
// unset.
//
// The ceiling exists because the bound is caller-supplied but the cost is paid
// by the server: the script runs synchronously inside the create request, so a
// caller-chosen timeout is really a caller-chosen hold on a connection. 55s
// keeps the whole create under the default 60s HTTP write timeout while still
// giving authors most of a minute, nearly double the default.
//
// Raising it requires raising --write-timeout to match; an operator who does
// both gets a longer setup budget, and one who raises only this gets a
// truncated response instead of a classified error, which main.go refuses at
// startup rather than discovering per request.
const DefaultMaxStartupScriptTimeout = 55 * time.Second

// EndpointReporter is implemented by environments that can report additional
// network endpoints published during StartAgent (e.g. via ingress/expose).
// Providers that don't support ingress simply omit it, in which case Boot
// reports no endpoints — consistent with how SnapshotCapable/SnapshotForkable
// are optional per-provider capabilities.
type EndpointReporter interface {
	Endpoints() []Endpoint
}

// Checkpoint is a snapshot of a sandbox.
type Checkpoint struct {
	ID        string
	Comment   string
	SizeBytes int64
	CreatedAt time.Time

	// Digest is the hex sha256 the host agent computed over the artifact as it
	// wrote it, or "" from an agent (or a backend) that does not hash. It
	// verifies a later transfer of these exact bytes and nothing else: two
	// builds of the same recipe produce different rootfs bytes, so it is never
	// an identity two artifacts can share and never a cache key.
	Digest string

	// Kind is what this checkpoint actually contains: rootfs only, or rootfs
	// plus guest memory. Backends that only ever take disk snapshots leave it
	// empty, which callers read as SnapshotKindDisk; empty is not an error,
	// it is an older agent (or a backend with nothing to distinguish).
	Kind SnapshotKind
}

// BootResult is returned after provisioning or restoring an environment.
type BootResult struct {
	Env                Environment
	BootTime           time.Duration
	FromCache          bool       // true if restored from checkpoint
	AuthTokenEncrypted []byte     // AES-GCM encrypted per-VM auth token for persistence
	DrainCommand       string     // graceful-shutdown command for the configured agent ('' => skip)
	Endpoints          []Endpoint // published endpoints, if the provider reported any
}

// reportedEndpoints returns env.Endpoints() when env implements
// EndpointReporter, or nil otherwise. Shared by bootFresh/bootRestore so a
// provider without ingress support (the common case today) contributes no
// endpoints rather than requiring every Environment to implement the method.
func reportedEndpoints(env Environment) []Endpoint {
	if er, ok := env.(EndpointReporter); ok {
		return er.Endpoints()
	}
	return nil
}

// bootInputs bundles the inputs needed by bootFresh and bootRestore.
// It exists purely to keep helper signatures readable.
type bootInputs struct {
	spec      Spec
	manifest  []byte
	secretMap map[string]string
	creds     *secrets.VMCredentials
	agentSpec AgentSpec
	opts      BootOptions
}

// maxConcurrentUploads bounds how many guest uploads are in flight at once.
//
// Every upload is an SSH session on the host agent, and until a Fusefile could
// carry a `copy` block the only caller was the agent profile with its three to
// six files, so the fan-out was unbounded because it could not get large. A
// copied source tree is hundreds of files, and opening hundreds of SSH sessions
// at a host agent at once is how a copy turns into a boot timeout.
const maxConcurrentUploads = 8

// uploadFiles uploads every path->bytes entry of files into the env,
// concurrently: each upload is an independent guest file with no shared
// state, so there is no reason to pay their round trips one after another.
// The generic boot path drives all guest uploads (manifest, secrets,
// credentials, and any caller files) through this from AgentSpec.Files; the
// agent profile decides which files exist and where they land.
func uploadFiles(ctx context.Context, env Environment, files map[string][]byte) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(files))
	sem := make(chan struct{}, maxConcurrentUploads)
	for path, data := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(path string, data []byte) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := env.Upload(ctx, data, path); err != nil {
				errs <- fmt.Errorf("upload %s: %w", path, err)
			}
		}(path, data)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	return nil
}

// setTokenIfSupported records the per-VM auth token on the env (when it
// implements TokenSetter) so env.Token() returns the active token without a
// separate fetch. Credential files themselves are uploaded via AgentSpec.Files
// (see FusedAgentSpec); this helper only handles the in-memory token side
// effect. Reused by Boot (fresh/restore) and RotateToken.
func setTokenIfSupported(env Environment, creds *secrets.VMCredentials) {
	if creds == nil {
		return
	}
	if ts, ok := env.(TokenSetter); ok {
		ts.SetToken(creds.AuthToken)
	}
}

// runStartupScript executes BootOptions.StartupScript inside the env, if set.
//
// The script is bounded by timeout (DefaultStartupScriptTimeout when zero).
// Boot runs synchronously inside the create request, so an unbounded script
// that never returns would hang the request until the HTTP write timeout cut
// the response, surfacing to the caller as an opaque 500 with a VM already
// running. Exceeding the bound is reported as ErrStartupScriptTimeout, which
// the API classifies as a client error.
func runStartupScript(ctx context.Context, env Environment, script string, timeout time.Duration) error {
	if script == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = DefaultStartupScriptTimeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := env.ExecStream(execCtx, io.Discard, io.Discard, "sh", "-lc", script); err != nil {
		// Only the bound we just imposed maps to the sentinel; a deadline
		// inherited from the caller's ctx is a genuine request timeout.
		if errors.Is(execCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("%w after %s: a startup script must terminate; "+
				"background long-lived processes or declare them under `services`",
				ErrStartupScriptTimeout, timeout)
		}
		return fmt.Errorf("startup script: %w", err)
	}
	return nil
}

// bootRestore attempts to restore env from its latest checkpoint and bring
// the guest agent back up. Returns (nil, nil) on a clean fall-through to fresh
// provision (e.g. provider not SnapshotCapable, no checkpoints, restore
// failed). Returns (result, nil) on successful restore. Returns (nil, err)
// only for hard errors that should abort Boot entirely (e.g. startup script
// failure).
func bootRestore(ctx context.Context, existing Environment, in bootInputs, start time.Time, encToken []byte) (*BootResult, error) {
	sc, ok := existing.(SnapshotCapable)
	if !ok {
		return nil, nil
	}
	checkpoints, cpErr := sc.ListCheckpoints(ctx)
	if cpErr != nil || len(checkpoints) == 0 {
		return nil, nil
	}
	latest := checkpoints[len(checkpoints)-1]
	if restoreErr := sc.Restore(ctx, latest.ID); restoreErr != nil {
		return nil, nil
	}

	// Re-upload the agent's files (manifest/secrets/credentials all live in
	// AgentSpec.Files, populated by the profile). Credential files carry the
	// SetToken side effect via setTokenIfSupported below.
	_ = uploadFiles(ctx, existing, in.agentSpec.Files)
	setTokenIfSupported(existing, in.creds)
	if err := runStartupScript(ctx, existing, in.opts.StartupScript, in.opts.StartupScriptTimeout); err != nil {
		return nil, err
	}
	_ = existing.StartAgent(ctx, in.agentSpec)
	return &BootResult{
		Env:                existing,
		BootTime:           time.Since(start),
		FromCache:          true,
		AuthTokenEncrypted: encToken,
		DrainCommand:       in.agentSpec.DrainCommand,
		Endpoints:          reportedEndpoints(existing),
	}, nil
}

// bootFresh provisions a new environment via p.Create, uploads the agent's
// files (manifest/secrets/credentials, all carried in AgentSpec.Files), runs
// the startup script, and starts the agent.
func bootFresh(ctx context.Context, p Provider, in bootInputs, start time.Time, encToken []byte) (*BootResult, error) {
	env, err := p.Create(ctx, in.spec)
	if err != nil {
		return nil, err
	}

	// Upload everything the agent profile declared. For fused this is the
	// manifest, secrets JSON, and (when present) the TLS/auth credential
	// files. The guest is responsible for mounting any sensitive paths on
	// tmpfs (see PRD-08 for the fused profile's /fuse contract).
	if err := uploadFiles(ctx, env, in.agentSpec.Files); err != nil {
		return nil, err
	}
	// Credential files were uploaded above; record the token on the env so
	// env.Token() returns it.
	setTokenIfSupported(env, in.creds)

	if err := runStartupScript(ctx, env, in.opts.StartupScript, in.opts.StartupScriptTimeout); err != nil {
		return nil, err
	}

	if err := env.StartAgent(ctx, in.agentSpec); err != nil {
		return nil, err
	}

	return &BootResult{
		Env:                env,
		BootTime:           time.Since(start),
		AuthTokenEncrypted: encToken,
		DrainCommand:       in.agentSpec.DrainCommand,
		Endpoints:          reportedEndpoints(env),
	}, nil
}

// Boot provisions or restores an environment, uploads the agent's files, and
// starts the configured guest agent. If a matching checkpoint exists, it
// restores from it. Otherwise creates fresh. When encryptionKey is non-nil (32
// bytes), per-VM TLS credentials and an auth token are generated and injected.
// The encrypted token is returned in BootResult for persistence.
//
// The guest agent is described by an AgentSpec; today that is always the fused
// profile (FusedAgentSpec), which carries the fused manifest/secrets/TLS files
// and launch command. Boot itself is profile-agnostic.
func Boot(ctx context.Context, p Provider, spec Spec, manifest []byte, secretMap map[string]string, opts BootOptions, encryptionKey []byte) (*BootResult, error) {
	start := time.Now()

	// Generate per-VM credentials when an encryption key is provided.
	var creds *secrets.VMCredentials
	var encToken []byte
	if len(encryptionKey) == 32 {
		var err error
		creds, err = secrets.GenerateVMCredentials(spec.Name)
		if err != nil {
			return nil, fmt.Errorf("generate vm credentials: %w", err)
		}
		encToken, err = secrets.EncryptToken(creds.AuthToken, encryptionKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt vm token: %w", err)
		}
	}

	// Build the fused profile: this is the SINGLE place the fused /fuse/*
	// paths and launch command are assembled (see agent_profile.go). Credential
	// files are included in AgentSpec.Files and uploaded generically; the
	// in-memory token side effect is applied by setTokenIfSupported in
	// bootFresh/bootRestore.
	agentSpec := FusedAgentSpec(manifest, secretMap, creds, opts)

	in := bootInputs{
		spec:      spec,
		manifest:  manifest,
		secretMap: secretMap,
		creds:     creds,
		agentSpec: agentSpec,
		opts:      opts,
	}

	// Try restoring from an existing checkpoint first. bootRestore returns
	// (nil, nil) when restore is not applicable (provider not SnapshotCapable,
	// no checkpoints, or restore failed) so we fall through to fresh provision.
	if existing, err := p.Get(ctx, spec.Name); err == nil {
		result, err := bootRestore(ctx, existing, in, start, encToken)
		if err != nil {
			return nil, err
		}
		if result != nil {
			return result, nil
		}
	}

	return bootFresh(ctx, p, in, start, encToken)
}
