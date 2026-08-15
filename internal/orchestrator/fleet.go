package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/folsomintel/fuse/internal/secrets"
)

// Sentinel errors exposed by the FleetManager public API. Callers
// (notably the REST handler layer) use errors.Is to map these to HTTP
// status codes without string-matching. These are kept intentionally
// coarse — they cover the cases that differ at the API boundary, not
// every internal failure mode.
var (
	// ErrTaskAlreadyAssigned is returned by ProvisionAndAssign when a
	// VM is already tracked for the given task id. The existing VM's
	// id is included in the wrapping fmt.Errorf context.
	ErrTaskAlreadyAssigned = errors.New("task already assigned")

	// ErrVMNotFound is returned by DestroyVM, CreateSnapshot,
	// RestoreSnapshot, and ListSnapshots when the vm id does not
	// match a tracked VM.
	ErrVMNotFound = errors.New("vm not found")

	// ErrTaskNotFound is returned by CompleteTask when the task id
	// does not match any VM.
	ErrTaskNotFound = errors.New("task not found")

	// ErrSnapshotNotFound is returned when a snapshot ID does not
	// correspond to a persisted snapshot record for the requested VM.
	ErrSnapshotNotFound = errors.New("snapshot not found")

	// ErrSnapshotQuotaExceeded is returned when creating a snapshot
	// would exceed the configured retention quota.
	ErrSnapshotQuotaExceeded = errors.New("snapshot quota exceeded")

	// ErrSnapshotInvalidState is returned when an operation targets a
	// snapshot that is not currently in the required lifecycle state.
	ErrSnapshotInvalidState = errors.New("snapshot invalid state")

	// ErrSnapshotHasChildren is returned when deleting a snapshot that
	// still has descendant snapshots in the lineage graph.
	ErrSnapshotHasChildren = errors.New("snapshot has children")

	// ErrVMNotRunning is returned by operations that require a VM to be
	// in the Running state (e.g. Drain). The current state is included
	// in the wrapping fmt.Errorf context so callers and operators can
	// see why the transition was rejected.
	ErrVMNotRunning = errors.New("vm not running")

	// ErrExecUnsupported is returned by Exec when the environment has no
	// real guest to run commands in (e.g. the in-memory stub a provider
	// falls back to when its BaseURL is unset). Reporting it is what keeps
	// a misconfigured host from answering exec with a fabricated success.
	ErrExecUnsupported = errors.New("exec not supported by provider")

	// ErrAttachUnsupported is returned by Attach when the environment does
	// not implement Attacher.
	ErrAttachUnsupported = errors.New("attach not supported by provider")

	// ErrSnapshotUnsupported is returned by CreateSnapshot and
	// RestoreSnapshot when the environment does not implement
	// SnapshotCapable. Like ErrExecUnsupported this is a provider
	// capability gap, not a server fault.
	ErrSnapshotUnsupported = errors.New("snapshot not supported by provider")

	// ErrGPUUnsupported is returned by CreateSnapshot and ForkEnvironment
	// when the target vm holds a gpu passthrough device. A vfio device
	// cannot be checkpointed (d4), so this is a permanent property of the
	// environment rather than a transient failure.
	ErrGPUUnsupported = errors.New("operation not supported for gpu environments")

	// ErrHostHasVMs is returned by RemoveHost when VMs are still assigned
	// to the host. Callers should cordon/drain and wait for the VMs to
	// leave before retrying.
	ErrHostHasVMs = errors.New("host still has vms assigned")

	// ErrStartupScriptTimeout is returned during boot when the caller's
	// startup script did not finish within StartupScriptTimeout. The boot
	// path runs the script synchronously, so a script that never returns
	// (a foreground server, `sleep infinity`) would otherwise hang the
	// create until the HTTP write timeout killed the response and the
	// caller saw an opaque 500. Long-lived processes belong in `services`,
	// or must be backgrounded by the script itself.
	ErrStartupScriptTimeout = errors.New("startup script did not complete in time")

	// Artifact placement and transfer conditions (ErrSeedUnplaceable,
	// ErrArtifactImmovable, ErrArtifactTransferFailed) are declared in
	// artifacts.go alongside the code that raises them, because two of the
	// three deliberately wrap ErrNoCapacity and that reasoning belongs next to
	// them rather than in a list.

	// ErrStartupScriptTimeoutTooLarge is returned by ProvisionAndAssign when
	// the caller asks for a startup script bound above the fleet's ceiling.
	// The request is refused rather than clamped: a caller that silently got
	// 55s when it asked for 10m would read the resulting
	// ErrStartupScriptTimeout as "my script is slow" instead of "my bound was
	// never honored".
	ErrStartupScriptTimeoutTooLarge = errors.New("startup script timeout exceeds the configured maximum")
)

// VMState represents the lifecycle state of a managed VM.
//
// Stored states (recorded on a vm and persisted to the state store):
//   - VMStateProvisioning, VMStateRunning, VMStateDraining, VMStateDestroying.
//
// Synthetic terminal states (emitted only over the wire as SSE events
// — never stored on a vm record because the underlying record is gone
// by the time we publish them): VMStateDestroyed, VMStateFailed. Defined
// alongside the wire format in events.go.
type VMState string

const (
	VMStateProvisioning VMState = "provisioning"
	VMStateRunning      VMState = "running"
	// VMStateDraining indicates the VM has been asked to gracefully quiesce
	// in-guest workloads (agent shutdown) but the VM itself has not yet
	// been destroyed. This is the "drain" phase of two-phase teardown:
	// the harness gets a clean shutdown signal and a chance to flush
	// outputs before the subsequent DELETE actually tears down the VM.
	// A Draining VM is still tracked, still consumes host capacity,
	// and may still be inspected via the API. DELETE is the only
	// supported state transition out of Draining (no "undrain").
	VMStateDraining   VMState = "draining"
	VMStateDestroying VMState = "destroying"
)

// vm is the internal representation of a tracked VM.
type vm struct {
	id                 string
	state              VMState
	taskID             string
	hostID             string // scheduler-assigned host, empty in single-provider mode
	env                Environment
	url                string // last-known reachable URL (cached for recovery before env is rehydrated)
	spec               Spec
	authTokenEncrypted []byte     // AES-GCM encrypted per-VM auth token
	secretsEncrypted   []byte     // AES-GCM encrypted JSON of the secret map (nil when no secrets supplied)
	drainCommand       string     // graceful-shutdown command run in the guest on Drain ('' => skip)
	endpoints          []Endpoint // published endpoints (e.g. ingress), if the provider reported any
	createdAt          time.Time
	updatedAt          time.Time
	// lastActivityAt is the clock the idle detector reads: the last time a
	// caller exec'd or attached to this VM (create counts as activity). It is
	// deliberately separate from updatedAt, which only moves on state
	// transitions and would make every healthy VM look permanently idle.
	// Not persisted: an orchestrator restart resets the idle window.
	lastActivityAt time.Time
	// attachSessions counts live attach streams. A VM holding at least one is
	// never idle, however quiet the stream is.
	attachSessions int
	err            string
	// healthcheck is the environment-level probe this VM was created with, or
	// nil when it was created without one. It is what makes reconcileHealth
	// poll this VM at all. Not persisted: it is only ever used to decide
	// whether to read a live signal back, and a restarted orchestrator that
	// stops polling a VM is a smaller problem than one that keeps reporting a
	// verdict nobody is refreshing.
	healthcheck *HealthcheckSpec
	// health is the last verdict read back from the guest, or nil before the
	// first successful read. Live state, never persisted (see health.go).
	health *HealthStatus
}

func (v *vm) toInfo() VMInfo {
	info := VMInfo{
		ID:        v.id,
		State:     v.state,
		TaskID:    v.taskID,
		HostID:    v.hostID,
		URL:       v.url,
		Spec:      v.spec,
		CreatedAt: v.createdAt,
		UpdatedAt: v.updatedAt,
		Error:     v.err,
		Endpoints: v.endpoints,
	}
	if v.env != nil {
		info.URL = v.env.URL()
	}
	// copied rather than aliased so a reader of the snapshot can never observe
	// the next reconcile tick rewriting the verdict underneath it.
	if v.health != nil {
		health := *v.health
		info.Health = &health
	}
	return info
}

// VMInfo is a read-only snapshot of a managed VM.
type VMInfo struct {
	ID        string
	State     VMState
	TaskID    string
	HostID    string
	URL       string
	Spec      Spec
	CreatedAt time.Time
	UpdatedAt time.Time
	Error     string
	Endpoints []Endpoint
	// Health is the environment-level probe's last verdict, or nil when the
	// environment declared no probe or none has been read back yet. See
	// health.go.
	Health *HealthStatus
}

// ReconcileMetrics is an optional callback invoked at the end of every
// reconcile cycle with a counter summary. Implementations typically feed a
// Prometheus/OTel exporter — the orchestrator package intentionally does not
// depend on either. A nil ReconcileMetrics disables metrics reporting.
type ReconcileMetrics interface {
	ReconcileCompleted(summary ReconcileSummary)
}

// ReconcileSummary captures counts from a single reconcile cycle.
type ReconcileSummary struct {
	TrackedVMs          int
	ProviderVMs         int
	OrphansDestroyed    int
	OrphansFailed       int
	OrphansDeadLettered int
	StuckTasksSuspected int
	StuckTasksFailed    int
	IdleVMsSuspected    int
	IdleVMsFailed       int
	VMsMissingProvider  int
	// HealthChecked counts VMs whose guest returned a usable health verdict
	// this cycle; HealthFailing is how many of those were failing. Neither
	// causes a teardown: the environment-level probe reports, it does not act.
	HealthChecked int
	HealthFailing int
	Duration      time.Duration
}

// FleetConfig configures the fleet manager.
type FleetConfig struct {
	Provider          Provider
	StateStore        StateStore
	Prefix            string        // VM name prefix, e.g. "fuse-"
	ReconcileInterval time.Duration // default 30s

	// TaskStuckTimeout is the maximum age of a Running VM with no state
	// transitions before it is considered stuck. This is a leak-detection
	// ceiling, NOT a heartbeat — healthy long-running tasks must set
	// Spec.MaxRuntime to override this. Default 2h.
	TaskStuckTimeout time.Duration

	// DefaultIdleTimeout applies to VMs whose Spec.IdleTimeout is zero. It is
	// how long a Running VM may go with no exec and no attach session before
	// it is destroyed. Unlike TaskStuckTimeout this is a liveness check, but
	// only over control-plane activity. Zero (the default) disables idle
	// expiry for VMs that do not ask for it.
	DefaultIdleTimeout time.Duration

	// OrphanDestroyMaxRetries is the number of consecutive reconcile cycles
	// an orphan VM may fail to destroy before being dead-lettered and
	// skipped on subsequent cycles. Default 5.
	OrphanDestroyMaxRetries int

	// StartupScriptTimeout bounds a caller's boot-time startup script when
	// BootOptions does not set its own. Default
	// DefaultStartupScriptTimeout. Keep it under the HTTP write timeout so
	// a create that trips it still returns a classified error.
	StartupScriptTimeout time.Duration

	// MaxStartupScriptTimeout is the largest bound a caller may request via
	// BootOptions.StartupScriptTimeout. Default
	// DefaultMaxStartupScriptTimeout. A request above it is refused with
	// ErrStartupScriptTimeoutTooLarge. Keep it under the HTTP write timeout
	// for the same reason StartupScriptTimeout is kept under it.
	MaxStartupScriptTimeout time.Duration

	// DefaultSnapshotRetention applies to snapshots created without an
	// explicit retention window. Zero leaves snapshots unbounded until
	// explicitly deleted.
	DefaultSnapshotRetention time.Duration

	// SnapshotQuotaMaxCount limits ready/in-flight snapshots per tenant.
	// Zero disables count-based enforcement.
	SnapshotQuotaMaxCount int

	// SnapshotQuotaMaxBytes limits the aggregate size of ready/in-flight
	// snapshots per tenant. Zero disables byte-based enforcement.
	SnapshotQuotaMaxBytes int64

	// ArtifactMover copies a build artifact from one host agent to another.
	// Nil (the default) means artifacts cannot move, in which case an
	// environment seeded from one is pinned to the host that holds it — the
	// behaviour that predates peer-to-peer distribution. See ArtifactMover.
	ArtifactMover ArtifactMover

	// ArtifactPullTimeout bounds a single host-to-host artifact transfer.
	// Zero inherits the caller's context, which for a create is the HTTP
	// request. Transfer time scales with artifact size and link speed, so any
	// fixed bound is either too small for the largest legitimate artifact or
	// too large to detect anything; set it only where the fleet's artifacts and
	// links are known.
	ArtifactPullTimeout time.Duration

	// ArtifactIdleTTL is how long a layer artifact may go unreferenced and
	// unused before the reconcile loop collects it. "Used" includes cache hits,
	// not just environments holding it open. Zero (the default) disables idle
	// collection: an orchestrator upgrade must not start deleting artifacts an
	// existing fleet relies on.
	ArtifactIdleTTL time.Duration

	// ArtifactMaxPerTenant caps how many ready layer artifacts one tenant may
	// keep, evicting least recently used on overflow. It is the cheap ceiling
	// behind ArtifactIdleTTL, for a tenant that builds often enough to keep
	// every layer inside the idle window. Zero (the default) disables it.
	ArtifactMaxPerTenant int

	// PlacementPolicy controls how the scheduler picks among hosts
	// when multiple are registered. Default is spread.
	PlacementPolicy PlacementPolicy

	// TokenEncryptionKey is the 32-byte AES-256 key used to encrypt
	// per-VM auth tokens before storing them in the state store.
	// When nil, VM credential generation is skipped (insecure mode).
	TokenEncryptionKey []byte

	// HostProviderFactory builds a Provider for a given (url, token,
	// backend) triple. The fleet uses it during recoverState to
	// rehydrate the per-host providers that RegisterHost would have
	// created during normal operation. Without it, an orchestrator
	// restart loses the scheduler's host registry and falls back to
	// single-provider mode for all subsequent placements.
	HostProviderFactory func(url, token string, backend HostBackend) Provider

	Metrics ReconcileMetrics
	Logger  *slog.Logger
}

// FleetManager tracks and manages a fleet of VMs.
type FleetManager struct {
	mu       sync.RWMutex
	provider Provider
	store    StateStore
	vms      map[string]*vm // keyed by VM ID
	prefix   string
	logger   *slog.Logger
	metrics  ReconcileMetrics

	startupScriptTimeout    time.Duration
	maxStartupScriptTimeout time.Duration

	reconcileInterval time.Duration

	taskStuckTimeout         time.Duration
	defaultIdleTimeout       time.Duration
	orphanDestroyMaxRetries  int
	defaultSnapshotRetention time.Duration
	snapshotQuotaMaxCount    int
	snapshotQuotaMaxBytes    int64

	// ── Content-addressed artifacts ──────────────────────────────
	//
	// The digest-to-hosts index is NOT here: it is derived from snapshot
	// records on demand (see artifacts.go), so there is nothing to keep in
	// sync and nothing to rehydrate at startup. Only the two genuinely
	// ephemeral facts live in memory.

	artifactMover        ArtifactMover
	artifactPullTimeout  time.Duration
	artifactIdleTTL      time.Duration
	artifactMaxPerTenant int

	// artifactUse is when each artifact was last resolved, seeded from, or
	// pulled, keyed by snapshot id. Not persisted: it would mean a database
	// write on every cache hit, and the collector falls back to the record's
	// own updated_at for anything this process has not seen, so a restart
	// loses nothing it can act on.
	artifactUse map[string]time.Time

	// artifactPulls counts in-flight transfers touching each snapshot id,
	// at BOTH ends. It is what stops the collector deleting an artifact that
	// is currently being read from or written to.
	artifactPulls map[string]int

	// orphanRetries tracks consecutive destroy failures per orphan VM
	// name. Cleared when the orphan disappears from the provider.
	orphanRetries map[string]int

	// stuckStrikes tracks how many consecutive reconcile cycles a VM has
	// been observed exceeding its runtime ceiling. A VM must accumulate
	// two strikes before it is failed, which guards against single-cycle
	// clock skew or transient scheduling pauses.
	stuckStrikes map[string]int

	// idleStrikes mirrors stuckStrikes for the idle detector: a VM must be
	// observed idle on two consecutive reconcile cycles before it is torn
	// down, so a single delayed cycle cannot kill a live environment.
	idleStrikes map[string]int

	// ── Scheduler / multi-host fields ────────────────────────────
	//
	// These are populated by RegisterHost and drive the Schedule()
	// placement function. When len(hosts)==0, ProvisionAndAssign
	// falls through to the single fm.provider path for backward compat.

	// hosts is the in-memory host registry, keyed by host ID.
	hosts map[string]*Host

	// hostProviders caches a Provider per registered host. The caller
	// (typically RegisterHost) supplies the concrete Provider for
	// each host; the FleetManager only holds the interface to avoid
	// an import cycle with provider packages.
	hostProviders map[string]Provider

	// tokenEncryptionKey is the 32-byte AES key for per-VM auth tokens.
	tokenEncryptionKey []byte

	// hostProviderFactory rebuilds Provider instances for hosts loaded
	// from the state store at startup. Nil disables host registry
	// recovery; in that case, recovered VMs fall through to the
	// single fm.provider path until an operator re-registers the host.
	hostProviderFactory func(url, token string, backend HostBackend) Provider

	// placementPolicy is the default scheduling strategy (binpack or
	// spread). Defaults to spread when empty.
	placementPolicy PlacementPolicy

	// broadcaster fans out per-VM state-change events to SSE
	// subscribers. Always non-nil — initialised in NewFleetManager.
	// See events.go for the contract (single-process pub/sub, no
	// cross-replica fanout, bounded per-subscriber buffer).
	broadcaster *eventBroadcaster

	cancel context.CancelFunc
	done   chan struct{}
}

// NewFleetManager creates a fleet manager. Call Start to begin the reconciliation loop.
func NewFleetManager(cfg FleetConfig) *FleetManager {
	if cfg.ReconcileInterval == 0 {
		cfg.ReconcileInterval = 30 * time.Second
	}
	if cfg.TaskStuckTimeout == 0 {
		cfg.TaskStuckTimeout = 2 * time.Hour
	}
	if cfg.OrphanDestroyMaxRetries == 0 {
		cfg.OrphanDestroyMaxRetries = 5
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.StateStore == nil {
		cfg.StateStore = NewMemoryStateStore()
	}
	if cfg.StartupScriptTimeout <= 0 {
		cfg.StartupScriptTimeout = DefaultStartupScriptTimeout
	}
	if cfg.MaxStartupScriptTimeout <= 0 {
		cfg.MaxStartupScriptTimeout = DefaultMaxStartupScriptTimeout
	}
	// A ceiling below the default would make the default itself unrequestable
	// and, worse, silently unreachable for callers that set nothing. Raise it
	// to the default rather than starting in a state where the two disagree.
	if cfg.MaxStartupScriptTimeout < cfg.StartupScriptTimeout {
		cfg.MaxStartupScriptTimeout = cfg.StartupScriptTimeout
	}

	return &FleetManager{
		provider:                 cfg.Provider,
		store:                    cfg.StateStore,
		vms:                      make(map[string]*vm),
		prefix:                   cfg.Prefix,
		logger:                   cfg.Logger,
		metrics:                  cfg.Metrics,
		startupScriptTimeout:     cfg.StartupScriptTimeout,
		maxStartupScriptTimeout:  cfg.MaxStartupScriptTimeout,
		reconcileInterval:        cfg.ReconcileInterval,
		taskStuckTimeout:         cfg.TaskStuckTimeout,
		defaultIdleTimeout:       cfg.DefaultIdleTimeout,
		orphanDestroyMaxRetries:  cfg.OrphanDestroyMaxRetries,
		defaultSnapshotRetention: cfg.DefaultSnapshotRetention,
		snapshotQuotaMaxCount:    cfg.SnapshotQuotaMaxCount,
		snapshotQuotaMaxBytes:    cfg.SnapshotQuotaMaxBytes,
		artifactMover:            cfg.ArtifactMover,
		artifactPullTimeout:      cfg.ArtifactPullTimeout,
		artifactIdleTTL:          cfg.ArtifactIdleTTL,
		artifactMaxPerTenant:     cfg.ArtifactMaxPerTenant,
		artifactUse:              make(map[string]time.Time),
		artifactPulls:            make(map[string]int),
		orphanRetries:            make(map[string]int),
		stuckStrikes:             make(map[string]int),
		idleStrikes:              make(map[string]int),
		hosts:                    make(map[string]*Host),
		hostProviders:            make(map[string]Provider),
		tokenEncryptionKey:       cfg.TokenEncryptionKey,
		hostProviderFactory:      cfg.HostProviderFactory,
		placementPolicy:          cfg.PlacementPolicy,
		broadcaster:              newEventBroadcaster(),
	}
}

// SubscribeEnvironmentEvents registers a subscriber for state-change
// events on a single VM. The returned channel receives events until
// cancel is called (idempotent) or the broadcaster drops the
// connection. Channel capacity is bounded; slow subscribers drop
// events with a logged warning rather than blocking publishers.
//
// This is a single-process broadcaster: events published on one
// orchestrator replica are not visible to subscribers on a different
// replica. The orchestrator runs as a single process today, so this
// is acceptable; cross-replica fanout would require a Redis or NATS
// backplane and is intentionally deferred.
func (fm *FleetManager) SubscribeEnvironmentEvents(vmID string) (<-chan EnvironmentEvent, func()) {
	return fm.broadcaster.subscribe(vmID)
}

// publishStateChange snapshots the current vm state under the read
// lock and dispatches an EnvironmentEvent to subscribers. It is the
// single entry point for state-change notifications: every state
// mutation site in fleet.go / drain.go / reconcile.go that flips
// v.state should call this (typically deferred until after the lock
// is released, so subscribers don't observe an inconsistent
// snapshot).
//
// Pass terminalState != "" to override the wire state for a removed
// VM. Once a VM is deleted from fm.vms there is no live record to
// snapshot, so callers (destroyAndRemove, reapDestroyedVMs,
// ProvisionAndAssign failure path) supply the terminal state
// explicitly.
func (fm *FleetManager) publishStateChange(vmID string, terminalState VMState) {
	if fm.broadcaster == nil {
		return
	}
	ev := EnvironmentEvent{
		ID:        NewEventID(),
		Kind:      "state",
		VMID:      vmID,
		UpdatedAt: time.Now(),
	}
	if terminalState != "" {
		ev.State = terminalState
	} else {
		fm.mu.RLock()
		v, ok := fm.vms[vmID]
		if !ok {
			fm.mu.RUnlock()
			return
		}
		info := v.toInfo()
		fm.mu.RUnlock()
		ev.State = info.State
		ev.URL = info.URL
		ev.Error = info.Error
		ev.UpdatedAt = info.UpdatedAt
	}
	_, dropped := fm.broadcaster.publish(ev)
	if dropped > 0 {
		fm.logger.Warn("dropped sse events for slow subscriber",
			"vm", vmID, "state", string(ev.State), "dropped", dropped,
		)
	}
}

// publishTerminalEvent emits a synthetic terminal-state event (e.g.
// "destroyed" or "failed") for a VM that has already been removed
// from fm.vms. The errMsg is included on the wire so subscribers can
// surface it to operators without a follow-up GET.
//
// This is split from publishStateChange because terminal events have
// no live vm record to snapshot — the caller has already deleted the
// map entry — and so the wire fields (state, error) must be passed
// in explicitly.
func (fm *FleetManager) publishTerminalEvent(vmID string, state VMState, errMsg string) {
	if fm.broadcaster == nil {
		return
	}
	ev := EnvironmentEvent{
		ID:        NewEventID(),
		Kind:      "state",
		VMID:      vmID,
		State:     state,
		Error:     errMsg,
		UpdatedAt: time.Now(),
	}
	_, dropped := fm.broadcaster.publish(ev)
	if dropped > 0 {
		fm.logger.Warn("dropped sse terminal event for slow subscriber",
			"vm", vmID, "state", string(state), "dropped", dropped,
		)
	}
}

// Start begins the background reconciliation loop.
func (fm *FleetManager) Start(ctx context.Context) {
	ctx, fm.cancel = context.WithCancel(ctx)
	fm.done = make(chan struct{})

	if err := fm.recoverState(ctx); err != nil {
		fm.logger.Error("state recovery failed", "err", err)
	}

	// Trigger a boot-time converge so recovered state is reconciled immediately.
	fm.reconcile(ctx)
	go fm.reconcileLoop(ctx)
}

// Stop cancels the reconciliation loop and waits for it to finish.
func (fm *FleetManager) Stop() {
	if fm.cancel != nil {
		fm.cancel()
	}
	if fm.done != nil {
		<-fm.done
	}
}

// ProvisionAndAssign provisions a new VM, boots fused, and assigns the given task.
// Blocks until the VM is ready or an error occurs.
func (fm *FleetManager) ProvisionAndAssign(ctx context.Context, taskID string, spec Spec, manifest []byte, secretMap map[string]string, opts BootOptions) (*VMInfo, error) {
	// Callers that do not pick their own bound inherit the fleet's, so a
	// startup script can never hang a create indefinitely. A caller that does
	// pick one is held to the fleet's ceiling, since the bound is really a
	// hold on the create connection.
	if opts.StartupScriptTimeout <= 0 {
		opts.StartupScriptTimeout = fm.startupScriptTimeout
	} else if opts.StartupScriptTimeout > fm.maxStartupScriptTimeout {
		return nil, fmt.Errorf("%w: requested %s, maximum is %s",
			ErrStartupScriptTimeoutTooLarge, opts.StartupScriptTimeout, fm.maxStartupScriptTimeout)
	}

	// Check for duplicate task.
	fm.mu.RLock()
	for _, v := range fm.vms {
		if v.taskID == taskID {
			existing := v.id
			fm.mu.RUnlock()
			return nil, fmt.Errorf("%w: task %s already assigned to vm %s", ErrTaskAlreadyAssigned, taskID, existing)
		}
	}
	fm.mu.RUnlock()

	vmID := fm.prefix + taskID
	spec.Name = vmID
	now := time.Now()

	v := &vm{
		id:             vmID,
		state:          VMStateProvisioning,
		taskID:         taskID,
		spec:           spec,
		createdAt:      now,
		updatedAt:      now,
		lastActivityAt: now,
		healthcheck:    opts.Healthcheck,
	}

	fm.mu.Lock()
	fm.vms[vmID] = v
	fm.mu.Unlock()
	if err := fm.persistVM(ctx, v); err != nil {
		fm.mu.Lock()
		delete(fm.vms, vmID)
		fm.mu.Unlock()
		return nil, fmt.Errorf("persist vm %s provisioning state: %w", vmID, err)
	}
	if err := fm.store.UpsertTask(ctx, TaskRecord{
		TaskID:     taskID,
		VMID:       vmID,
		RunStatus:  TaskRunAssigned,
		AssignedAt: now,
		UpdatedAt:  now,
	}); err != nil {
		fm.mu.Lock()
		delete(fm.vms, vmID)
		fm.mu.Unlock()
		_ = fm.store.DeleteVM(ctx, vmID)
		return nil, fmt.Errorf("persist task %s assigned state: %w", taskID, err)
	}
	fm.appendEvent(ctx, "vm", vmID, "vm.provisioning", map[string]any{"task_id": taskID})
	fm.appendEvent(ctx, "task", taskID, "task.assigned", map[string]any{"vm_id": vmID})
	fm.publishStateChange(vmID, "")

	fm.logger.Info("provisioning vm", "vm", vmID, "task", taskID)

	// Resolve where this spec's seed artifact already lives, before any lock is
	// taken: it reads the state store, and the scheduler runs under fm.mu.
	// An artifact that can move turns the old hard pin into a preference; one
	// that cannot keeps the pin, because scheduling somewhere the artifact can
	// never arrive is worse than not scheduling at all.
	seed := fm.planSeedPlacement(ctx, spec)

	// Select and reserve a host before boot so concurrent provisions cannot
	// claim the same capacity. CPU-only legacy deployments may still use the
	// default provider when no hosts are registered.
	bootProvider := fm.provider
	reservedHost := false
	fm.mu.Lock()
	hosts := fm.activeHostsLocked()
	// A vm seeded from an immovable artifact can only be born where those bytes
	// already are, so narrow the candidate set to that host and let Schedule
	// report insufficient capacity the way it does for any other request.
	// Narrowing the input rather than bypassing Schedule keeps one scheduling
	// path. When the artifact CAN move, this narrowing is exactly the failure
	// mode being removed: the pin becomes a locality preference below instead.
	if spec.PinnedHostID != "" && !seed.movable {
		pinned := make([]*Host, 0, 1)
		for _, h := range hosts {
			if h.ID == spec.PinnedHostID {
				pinned = append(pinned, h)
			}
		}
		if len(pinned) == 0 {
			delete(fm.vms, vmID)
			fm.mu.Unlock()
			_ = fm.store.DeleteVM(ctx, vmID)
			return nil, fmt.Errorf("%w: host %s is required by the seed snapshot but is not active (%s)",
				ErrNoCapacity, spec.PinnedHostID, seed.whyImmovable())
		}
		hosts = pinned
	}
	if len(hosts) == 0 && spec.GPUs > 0 {
		delete(fm.vms, vmID)
		fm.mu.Unlock()
		_ = fm.store.DeleteVM(ctx, vmID)
		return nil, fmt.Errorf("%w: gpu workloads require a registered gpu host", ErrNoCapacity)
	}
	// A placement request cannot be honored on the legacy no-hosts path: the
	// default provider is not a scheduled host, so a pin or a label selector
	// there would silently boot somewhere else.
	if len(hosts) == 0 && (spec.HostID != "" || len(spec.Labels) > 0) {
		delete(fm.vms, vmID)
		fm.mu.Unlock()
		_ = fm.store.DeleteVM(ctx, vmID)
		return nil, fmt.Errorf("%w: placement requires registered hosts", ErrNoHosts)
	}
	if len(hosts) > 0 {
		selectedHost, decision, schedErr := SchedulePreferring(spec, hosts, fm.placementPolicy,
			PlacementHints{ArtifactHosts: seed.hostPreference()})
		if schedErr != nil {
			delete(fm.vms, vmID)
			fm.mu.Unlock()
			_ = fm.store.DeleteVM(ctx, vmID)
			// Name every rung that was tried. The operator asked for an
			// environment, not for a particular host, so "no capacity" alone
			// leaves them unable to tell a full fleet from an artifact that
			// could not follow the workload.
			if spec.SeedSnapshotID != "" {
				return nil, fmt.Errorf("%w: seeded from %s: %w", ErrSeedUnplaceable, seed.describe(), schedErr)
			}
			return nil, schedErr
		}
		v.hostID = selectedHost.ID
		hp, hpOK := fm.providerForHost(selectedHost.ID)
		if !hpOK {
			delete(fm.vms, vmID)
			fm.mu.Unlock()
			_ = fm.store.DeleteVM(ctx, vmID)
			return nil, fmt.Errorf("provider for host %s not found after scheduling", selectedHost.ID)
		}
		bootProvider = hp
		fm.allocateOnHost(selectedHost.ID, v)
		// allocateOnHost binds concrete gpu device uuids onto v.spec; mirror
		// them onto the local spec so Boot and the reservation-release path
		// see the same binding. the same applies to per-instance MIG uuids.
		spec.GPUUUIDs = v.spec.GPUUUIDs
		spec.MIGInstanceUUIDs = v.spec.MIGInstanceUUIDs
		reservedHost = true
		fm.mu.Unlock()
		fm.logger.Info("scheduled vm",
			"vm", vmID, "host", selectedHost.ID,
			"policy", decision.Policy,
			"candidates", decision.Candidates,
			"headroom_cpus", decision.HeadroomCPUs,
			"artifact_local", decision.ArtifactLocal,
		)
		fm.appendEvent(ctx, "vm", vmID, "vm.scheduled", map[string]any{
			"host_id":        decision.HostID,
			"policy":         string(decision.Policy),
			"candidates":     decision.Candidates,
			"artifact_local": decision.ArtifactLocal,
		})
	} else {
		fm.mu.Unlock()
	}
	releaseReservation := func() {
		if !reservedHost {
			return
		}
		fm.mu.Lock()
		fm.deallocateOnHost(v.hostID, spec)
		fm.mu.Unlock()
		reservedHost = false
	}
	abandonProvision := func(cause error) error {
		releaseReservation()
		fm.mu.Lock()
		v.err = cause.Error()
		delete(fm.vms, vmID)
		fm.mu.Unlock()
		_ = fm.store.DeleteVM(ctx, vmID)
		fm.upsertTaskBackground(TaskRecord{
			TaskID:     taskID,
			VMID:       vmID,
			RunStatus:  TaskRunFailed,
			LastError:  cause.Error(),
			AssignedAt: now,
			UpdatedAt:  time.Now(),
		})
		fm.publishTerminalEvent(vmID, VMStateFailed, cause.Error())
		return cause
	}

	// The workload was placed somewhere that does not hold the artifact, so the
	// artifact follows it. This happens BEFORE the VM is created: the host agent
	// resolves a seed id out of its own snapshot store, so a create issued
	// before the bytes land would fail with a bare "snapshot not found".
	if seed.needsMove(v.hostID) {
		localID, mvErr := fm.ensureArtifactOnHost(ctx, seed.record, v.hostID)
		if mvErr != nil {
			return nil, abandonProvision(fmt.Errorf("provision vm %s: %w", vmID, mvErr))
		}
		// The seed id is per host, so the create must name the copy that landed
		// here rather than the one the caller resolved. Both the local spec
		// (which Boot reads) and the tracked vm's spec (which is persisted and
		// which the collector reads to see the artifact is in use) are updated.
		fm.mu.Lock()
		spec.SeedSnapshotID = localID
		spec.PinnedHostID = v.hostID
		v.spec.SeedSnapshotID = localID
		v.spec.PinnedHostID = v.hostID
		fm.mu.Unlock()
	}
	// Seeding from an artifact is a use of it. Recording that here, rather than
	// only on a cache lookup, is what keeps an artifact that many environments
	// boot from out of the collector's reach even when no lookup has touched it
	// for a while.
	if spec.SeedSnapshotID != "" {
		fm.touchArtifact(spec.SeedSnapshotID, seed.record.SnapshotID)
	}

	result, err := Boot(ctx, bootProvider, spec, manifest, secretMap, opts, fm.tokenEncryptionKey)
	if err != nil {
		releaseReservation()
		redactedErr := secrets.RedactSecretValues(err.Error(), secretMap)
		fm.logger.Error("provision failed", "vm", vmID, "task", taskID, "err", redactedErr)

		fm.mu.Lock()
		v.err = redactedErr
		v.updatedAt = time.Now()
		delete(fm.vms, vmID)
		fm.mu.Unlock()
		fm.upsertTaskBackground(TaskRecord{
			TaskID:     taskID,
			VMID:       vmID,
			RunStatus:  TaskRunFailed,
			RetryCount: 0,
			LastError:  redactedErr,
			AssignedAt: now,
			UpdatedAt:  time.Now(),
		})
		fm.deleteVMBackground(vmID)
		fm.appendEventBackground("vm", vmID, "vm.provision_failed", map[string]any{"task_id": taskID, "error": redactedErr})
		fm.appendEventBackground("task", taskID, "task.failed", map[string]any{"vm_id": vmID, "error": redactedErr})
		// Synthetic terminal state — the vm has already been removed
		// from fm.vms above so subscribers must learn of the failure
		// from this synthesised event rather than from a snapshot.
		fm.publishTerminalEvent(vmID, VMStateFailed, redactedErr)

		// Best-effort cleanup.
		// TODO: Track this goroutine so Stop() can wait for it to finish
		// instead of leaking on shutdown.
		go func() {
			if destroyErr := bootProvider.Destroy(context.Background(), vmID); destroyErr != nil {
				fm.logger.Error("cleanup destroy failed", "vm", vmID, "err", destroyErr)
			}
		}()

		return nil, fmt.Errorf("provision vm %s: %w", vmID, err)
	}

	// Persist secrets encrypted under the orchestrator KEK so a crashed
	// orchestrator can re-upload them to the guest on recovery. Stored
	// only when both an encryption key is configured and the caller
	// supplied at least one secret. Failure here is non-fatal: the VM
	// is already running and serving traffic; we degrade to the legacy
	// "no recovery for secrets" behavior and log loudly.
	var secretsEncrypted []byte
	if len(secretMap) > 0 && len(fm.tokenEncryptionKey) == 32 {
		secretsJSON, mErr := json.Marshal(secretMap)
		if mErr != nil {
			fm.logger.Warn("marshal secrets for persistence failed", "vm", vmID, "err", mErr)
		} else {
			ct, encErr := secrets.EncryptBytes(secretsJSON, fm.tokenEncryptionKey)
			if encErr != nil {
				fm.logger.Warn("encrypt secrets for persistence failed", "vm", vmID, "err", encErr)
			} else {
				secretsEncrypted = ct
			}
		}
	}

	fm.mu.Lock()
	v.env = result.Env
	v.url = result.Env.URL()
	v.state = VMStateRunning
	v.authTokenEncrypted = result.AuthTokenEncrypted
	v.secretsEncrypted = secretsEncrypted
	v.drainCommand = result.DrainCommand
	v.endpoints = result.Endpoints
	v.updatedAt = time.Now()
	fm.mu.Unlock()
	fm.publishStateChange(vmID, "")

	fm.logger.Info("vm running", "vm", vmID, "task", taskID, "boot_time", result.BootTime, "from_cache", result.FromCache, "host", v.hostID)
	if err := fm.persistVMByID(ctx, vmID); err != nil {
		releaseReservation()
		fm.logger.Error("persist running state failed, rolling back", "vm", vmID, "task", taskID, "err", err)

		fm.mu.Lock()
		delete(fm.vms, vmID)
		fm.mu.Unlock()
		errMsg := fmt.Sprintf("persist vm running state failed: %v", err)
		fm.upsertTaskBackground(TaskRecord{
			TaskID:     taskID,
			VMID:       vmID,
			RunStatus:  TaskRunFailed,
			RetryCount: 0,
			LastError:  errMsg,
			AssignedAt: now,
			UpdatedAt:  time.Now(),
		})
		fm.deleteVMBackground(vmID)
		fm.publishTerminalEvent(vmID, VMStateFailed, errMsg)

		go func() {
			if destroyErr := bootProvider.Destroy(context.Background(), vmID); destroyErr != nil {
				fm.logger.Error("cleanup destroy failed after persist error", "vm", vmID, "err", destroyErr)
			}
		}()

		return nil, fmt.Errorf("persist vm %s running state: %w", vmID, err)
	}
	if err := fm.store.UpsertTask(ctx, TaskRecord{
		TaskID:     taskID,
		VMID:       vmID,
		RunStatus:  TaskRunRunning,
		RetryCount: 0,
		LastError:  "",
		AssignedAt: now,
		UpdatedAt:  time.Now(),
	}); err != nil {
		releaseReservation()
		fm.logger.Error("persist task running state failed, rolling back", "vm", vmID, "task", taskID, "err", err)

		fm.mu.Lock()
		delete(fm.vms, vmID)
		fm.mu.Unlock()
		errMsg := fmt.Sprintf("persist task running state failed: %v", err)
		fm.upsertTaskBackground(TaskRecord{
			TaskID:     taskID,
			VMID:       vmID,
			RunStatus:  TaskRunFailed,
			RetryCount: 0,
			LastError:  errMsg,
			AssignedAt: now,
			UpdatedAt:  time.Now(),
		})
		fm.deleteVMBackground(vmID)
		fm.publishTerminalEvent(vmID, VMStateFailed, errMsg)

		go func() {
			if destroyErr := bootProvider.Destroy(context.Background(), vmID); destroyErr != nil {
				fm.logger.Error("cleanup destroy failed after persist error", "vm", vmID, "err", destroyErr)
			}
		}()

		return nil, fmt.Errorf("persist task %s running state: %w", taskID, err)
	}
	fm.appendEvent(ctx, "vm", vmID, "vm.running", map[string]any{"task_id": taskID})
	fm.appendEvent(ctx, "task", taskID, "task.running", map[string]any{"vm_id": vmID})
	if len(secretMap) > 0 {
		fm.appendEvent(ctx, "vm", vmID, "secrets.deployed", map[string]any{
			"task_id":     taskID,
			"secret_keys": secrets.SecretKeyNames(secretMap),
			"count":       len(secretMap),
		})
	}

	info := v.toInfo()
	return &info, nil
}

// CompleteTask marks a task as done and triggers async VM destruction.
func (fm *FleetManager) CompleteTask(taskID string) error {
	fm.mu.Lock()
	var found *vm
	for _, v := range fm.vms {
		if v.taskID == taskID {
			found = v
			break
		}
	}
	if found == nil {
		fm.mu.Unlock()
		return fmt.Errorf("%w: no vm found for task %s", ErrTaskNotFound, taskID)
	}
	prevState := found.state
	prevTaskID := found.taskID
	prevUpdatedAt := found.updatedAt

	found.state = VMStateDestroying
	found.taskID = ""
	found.updatedAt = time.Now()
	vmID := found.id
	assignedAt := found.createdAt
	fm.mu.Unlock()

	rollback := func() {
		fm.mu.Lock()
		current, ok := fm.vms[vmID]
		if ok {
			current.state = prevState
			current.taskID = prevTaskID
			current.updatedAt = prevUpdatedAt
		}
		fm.mu.Unlock()
		if ok {
			if err := fm.persistVMByID(context.Background(), vmID); err != nil {
				fm.logger.Warn("failed to persist vm rollback after complete task failure", "vm", vmID, "err", err)
			}
		}
	}

	if err := fm.persistVMByID(context.Background(), vmID); err != nil {
		rollback()
		return fmt.Errorf("persist vm %s destroying state: %w", vmID, err)
	}
	if err := fm.store.UpsertTask(context.Background(), TaskRecord{
		TaskID:     taskID,
		VMID:       vmID,
		RunStatus:  TaskRunCompleted,
		RetryCount: 0,
		LastError:  "",
		AssignedAt: assignedAt,
		UpdatedAt:  time.Now(),
	}); err != nil {
		rollback()
		return fmt.Errorf("persist task %s completion state: %w", taskID, err)
	}
	fm.appendEvent(context.Background(), "task", taskID, "task.completed", map[string]any{"vm_id": vmID})
	fm.appendEvent(context.Background(), "vm", vmID, "vm.destroying", map[string]any{"task_id": taskID})
	fm.publishStateChange(vmID, "")

	fm.logger.Info("completing task, destroying vm", "vm", found.id, "task", taskID)
	go fm.destroyAndRemove(vmID)

	return nil
}

// DestroyVM forcefully tears down a VM by ID.
func (fm *FleetManager) DestroyVM(ctx context.Context, vmID string) error {
	fm.mu.Lock()
	v, ok := fm.vms[vmID]
	if !ok {
		fm.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	prevState := v.state
	priorTaskID := v.taskID
	assignedAt := v.createdAt
	prevUpdatedAt := v.updatedAt
	provider, providerOK := fm.providerForHost(v.hostID)
	if v.hostID == "" {
		provider = fm.provider
		providerOK = true
	}
	if !providerOK {
		fm.mu.Unlock()
		return fmt.Errorf("provider for host %s not found", v.hostID)
	}
	v.state = VMStateDestroying
	v.taskID = ""
	v.updatedAt = time.Now()
	fm.mu.Unlock()

	rollback := func() {
		fm.mu.Lock()
		current, exists := fm.vms[vmID]
		if exists {
			current.state = prevState
			current.taskID = priorTaskID
			current.updatedAt = prevUpdatedAt
		}
		fm.mu.Unlock()
		if exists {
			if err := fm.persistVMByID(context.Background(), vmID); err != nil {
				fm.logger.Warn("failed to persist vm rollback after destroy failure", "vm", vmID, "err", err)
			}
		}
	}

	if err := fm.persistVMByID(ctx, vmID); err != nil {
		rollback()
		return fmt.Errorf("persist vm %s destroying state: %w", vmID, err)
	}
	if priorTaskID != "" {
		if err := fm.store.UpsertTask(ctx, TaskRecord{
			TaskID:     priorTaskID,
			VMID:       vmID,
			RunStatus:  TaskRunFailed,
			RetryCount: 0,
			LastError:  "vm force destroyed",
			AssignedAt: assignedAt,
			UpdatedAt:  time.Now(),
		}); err != nil {
			rollback()
			return fmt.Errorf("persist task %s destroy state: %w", priorTaskID, err)
		}
		fm.appendEvent(ctx, "task", priorTaskID, "task.failed", map[string]any{"vm_id": vmID, "error": "vm force destroyed"})
	}
	fm.appendEvent(ctx, "vm", vmID, "vm.destroying", map[string]any{"reason": "manual_destroy"})
	fm.publishStateChange(vmID, "")

	fm.logger.Info("destroying vm", "vm", vmID)

	if err := provider.Destroy(ctx, vmID); err != nil {
		fm.logger.Error("destroy failed", "vm", vmID, "err", err)
		return fmt.Errorf("destroy vm %s: %w", vmID, err)
	}

	fm.mu.Lock()
	if v.hostID != "" {
		fm.deallocateOnHost(v.hostID, v.spec)
	}
	delete(fm.vms, vmID)
	fm.mu.Unlock()
	fm.deleteVMBackground(vmID)
	fm.appendEventBackground("vm", vmID, "vm.removed", map[string]any{"reason": "destroyed"})
	fm.publishTerminalEvent(vmID, VMStateDestroyed, "")

	return nil
}

// VMFilter narrows ListFleetFiltered results on exact-match fields, plus
// pagination controls.
//
// Limit caps the page size (<=0 defaults to DefaultPageLimit, >MaxPageLimit
// is clamped down to it). Cursor, when non-empty, resumes after the last
// item of a previous page (see ListFleetFiltered's NextCursor return).
type VMFilter struct {
	TaskID string
	State  VMState
	HostID string
	Limit  int
	Cursor string
}

// vmCursor is the opaque cursor payload for VM listing. VM ids are unique
// and stable for the life of a VM, so an ascending id sort is a simple,
// stable pagination key — no separate sequence column needed.
type vmCursor struct {
	ID string `json:"id"`
}

// ListFleet returns every tracked VM matching no filter, transparently
// paginating through the full result set. Intended for internal/test
// callers that want "everything" rather than one page; the REST API uses
// ListFleetFiltered directly so it can return a cursor to the caller.
func (fm *FleetManager) ListFleet() []VMInfo {
	var out []VMInfo
	cursor := ""
	for {
		page, next, err := fm.ListFleetFiltered(VMFilter{Limit: MaxPageLimit, Cursor: cursor})
		if err != nil {
			// cursor is always well-formed here (we generated it ourselves),
			// so this should be unreachable; bail out defensively.
			return out
		}
		out = append(out, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	return out
}

// ListFleetFiltered returns one page of tracked VMs filtered by optional
// exact-match fields, sorted ascending by VM id. It returns the page, a
// next-page cursor (empty when there are no more results), and an error
// only for a malformed cursor.
//
// The filter+copy pass runs under fm.mu.RLock (reading vm fields is only
// safe under lock); sorting and windowing the resulting VMInfo copies runs
// after the lock is released, since those are plain value data by then.
func (fm *FleetManager) ListFleetFiltered(filter VMFilter) ([]VMInfo, string, error) {
	limit := clampLimit(filter.Limit)

	var after string
	if filter.Cursor != "" {
		var c vmCursor
		if err := decodeCursor(filter.Cursor, &c); err != nil {
			return nil, "", err
		}
		after = c.ID
	}

	fm.mu.RLock()
	matched := make([]VMInfo, 0, len(fm.vms))
	for _, v := range fm.vms {
		if filter.TaskID != "" && v.taskID != filter.TaskID {
			continue
		}
		if filter.State != "" && v.state != filter.State {
			continue
		}
		if filter.HostID != "" && v.hostID != filter.HostID {
			continue
		}
		matched = append(matched, v.toInfo())
	}
	fm.mu.RUnlock()

	sort.Slice(matched, func(i, j int) bool { return matched[i].ID < matched[j].ID })

	start := 0
	if after != "" {
		start = sort.Search(len(matched), func(i int) bool { return matched[i].ID > after })
	}

	end := len(matched)
	var next string
	if start+limit < end {
		end = start + limit
		next, _ = encodeCursor(vmCursor{ID: matched[end-1].ID})
	}
	if start > end {
		start = end
	}
	return matched[start:end], next, nil
}

// GetVM returns info for a specific VM.
func (fm *FleetManager) GetVM(vmID string) (VMInfo, bool) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	v, ok := fm.vms[vmID]
	if !ok {
		return VMInfo{}, false
	}
	return v.toInfo(), true
}

// GetVMByTask returns the VM assigned to a given task.
func (fm *FleetManager) GetVMByTask(taskID string) (VMInfo, bool) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	for _, v := range fm.vms {
		if v.taskID == taskID {
			return v.toInfo(), true
		}
	}
	return VMInfo{}, false
}

func (fm *FleetManager) reconcileLoop(ctx context.Context) {
	defer close(fm.done)

	tick := time.NewTicker(fm.reconcileInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			fm.reconcile(ctx)
		}
	}
}

func (fm *FleetManager) reconcile(ctx context.Context) {
	start := time.Now()
	summary := ReconcileSummary{}

	// Multi-host mode: list VMs from all registered host providers.
	// Single-provider fallback: list from fm.provider (legacy path).
	var envs []Environment
	envProviders := make(map[string]Provider)
	var listErr error
	fm.mu.RLock()
	multiHost := len(fm.hosts) > 0
	fm.mu.RUnlock()
	if multiHost {
		envs, envProviders, listErr = fm.listAllHostVMs(ctx)
	} else {
		envs, listErr = fm.provider.List(ctx, fm.prefix)
		for _, env := range envs {
			envProviders[env.Name()] = fm.provider
		}
	}
	if listErr != nil {
		fm.logger.Error("reconcile list failed", "err", listErr)
		if fm.metrics != nil {
			summary.Duration = time.Since(start)
			fm.metrics.ReconcileCompleted(summary)
		}
		return
	}

	providerVMs := make(map[string]bool, len(envs))
	for _, e := range envs {
		providerVMs[e.Name()] = true
	}
	summary.ProviderVMs = len(providerVMs)

	fm.mu.Lock()
	summary.TrackedVMs = len(fm.vms)
	// Detect VMs we track that vanished from the provider.
	changedVMs := make([]string, 0)
	failedTasks := make([]TaskRecord, 0)
	for id, v := range fm.vms {
		if v.state == VMStateProvisioning || v.state == VMStateDestroying {
			continue
		}
		if !providerVMs[id] {
			fm.logger.Warn("vm missing from provider, marking destroying", "vm", id)
			oldTaskID := v.taskID
			v.state = VMStateDestroying
			v.taskID = ""
			v.err = "vm missing from provider"
			v.updatedAt = time.Now()
			changedVMs = append(changedVMs, id)
			summary.VMsMissingProvider++
			if oldTaskID != "" {
				failedTasks = append(failedTasks, TaskRecord{
					TaskID:     oldTaskID,
					VMID:       id,
					RunStatus:  TaskRunFailed,
					RetryCount: 0,
					LastError:  "vm missing from provider",
					AssignedAt: v.createdAt,
					UpdatedAt:  time.Now(),
				})
			}
		}
	}

	// Build set of tracked IDs for orphan detection.
	tracked := make(map[string]bool, len(fm.vms))
	for id := range fm.vms {
		tracked[id] = true
	}
	fm.mu.Unlock()
	for _, vmID := range changedVMs {
		fm.persistVMBackground(vmID)
		fm.appendEventBackground("vm", vmID, "vm.destroying", map[string]any{"reason": "missing_from_provider"})
		fm.publishStateChange(vmID, "")
	}
	for _, task := range failedTasks {
		fm.upsertTaskBackground(task)
		fm.appendEventBackground("task", task.TaskID, "task.failed", map[string]any{"vm_id": task.VMID, "error": task.LastError})
	}

	// Handle orphans and stuck tasks in their own pass. These helpers live
	// in reconcile.go so that fleet.go stays focused on the lifecycle API.
	fm.reconcileOrphans(ctx, envs, envProviders, tracked, &summary)
	fm.reconcileStuckTasks(ctx, &summary)
	fm.reconcileIdleVMs(ctx, &summary)
	fm.reconcileHealth(ctx, &summary)
	fm.reconcileSnapshots(ctx)
	fm.reconcileArtifacts(ctx)

	// Sweep VMs that have been marked destroying and are no longer present
	// in the provider so they don't accumulate in memory or in the store.
	fm.reapDestroyedVMs(providerVMs)

	// Heal host allocation drift when an orphan is destroyed (#68). An orphan
	// is untracked by definition, so it has no persisted Spec for
	// deallocateOnHost to subtract; its allocation (if a crash that lost the
	// vm record left a stale counter behind) is only correctable by re-deriving
	// each host's Allocated from the live fleet. That is safe here only when no
	// VM is mid-teardown, because recompute (like startup recovery) excludes
	// Destroying VMs that may still hold a device not yet reaped by
	// reapDestroyedVMs above.
	if summary.OrphansDestroyed > 0 && !fm.hasTrackedDestroyingVM() {
		for _, h := range fm.recomputeHostAllocations() {
			fm.persistHostRecordBackground(fm.hostToRecord(h))
		}
	}

	if fm.metrics != nil {
		summary.Duration = time.Since(start)
		fm.metrics.ReconcileCompleted(summary)
	}
}

func (fm *FleetManager) destroyAndRemove(vmID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	hostID := ""
	if ok {
		hostID = v.hostID
	}
	fm.mu.RUnlock()
	provider, err := fm.providerForVM(hostID)
	if err != nil {
		fm.logger.Error("async destroy provider lookup failed", "vm", vmID, "err", err)
		return
	}
	if err := provider.Destroy(ctx, vmID); err != nil {
		fm.logger.Error("async destroy failed", "vm", vmID, "err", err)
		return
	}

	fm.mu.Lock()
	v, ok = fm.vms[vmID]
	if ok {
		if v.hostID != "" {
			fm.deallocateOnHost(v.hostID, v.spec)
		}
		delete(fm.vms, vmID)
	}
	fm.mu.Unlock()
	fm.deleteVMBackground(vmID)
	fm.appendEventBackground("vm", vmID, "vm.removed", map[string]any{"reason": "async_destroy"})
	fm.publishTerminalEvent(vmID, VMStateDestroyed, "")
}

// reapDestroyedVMs removes tracked VMs that are in the Destroying state
// and are no longer present in the provider (set provided by reconcile).
// VM resources are returned to their host, the VM is removed from the
// in-memory fleet, and the persisted record is deleted.
func (fm *FleetManager) reapDestroyedVMs(providerVMs map[string]bool) {
	fm.mu.Lock()
	toRemove := make([]string, 0)
	for id, v := range fm.vms {
		if v.state != VMStateDestroying {
			continue
		}
		if providerVMs[id] {
			continue
		}
		if v.hostID != "" {
			fm.deallocateOnHost(v.hostID, v.spec)
		}
		delete(fm.vms, id)
		toRemove = append(toRemove, id)
	}
	fm.mu.Unlock()
	for _, id := range toRemove {
		fm.deleteVMBackground(id)
		fm.appendEventBackground("vm", id, "vm.removed", map[string]any{"reason": "destroyed"})
		fm.publishTerminalEvent(id, VMStateDestroyed, "")
	}
}

// reattachVMFromProvider re-establishes a recovered VM's live environment by
// looking it up in its provider (single-provider or per-host) and restoring
// env/url plus the decrypted auth token. It does not mutate state; callers
// decide the target state based on whether the lookup succeeds. Shared by the
// Running and Provisioning recovery branches so an interrupted provision is
// re-attached rather than destroyed (#68).
func (fm *FleetManager) reattachVMFromProvider(ctx context.Context, v *vm) error {
	lookupProvider, providerErr := fm.providerForVM(v.hostID)
	if providerErr != nil {
		return providerErr
	}
	env, err := lookupProvider.Get(ctx, v.id)
	if err != nil {
		return err
	}
	v.env = env
	v.url = env.URL()
	if len(v.authTokenEncrypted) > 0 && len(fm.tokenEncryptionKey) == 32 {
		if plain, decErr := secrets.DecryptToken(v.authTokenEncrypted, fm.tokenEncryptionKey); decErr == nil {
			if ts, ok := env.(TokenSetter); ok {
				ts.SetToken(plain)
			}
		} else {
			fm.logger.Warn("recover: decrypt auth token failed", "vm", v.id, "err", decErr)
		}
	}
	return nil
}

// recoverState rehydrates the fleet from the state store after a restart.
//
// Note what is deliberately absent: there is no artifact-index phase. The
// digest-to-hosts mapping is derived from snapshot rows on demand (see
// artifacts.go), so there is nothing to rebuild and nothing that can come back
// wrong. Artifact use times are not recovered either; the collector falls back
// to each record's own updated_at for anything this process has not seen used,
// so a restart cannot make an artifact suddenly look collectable.
func (fm *FleetManager) recoverState(ctx context.Context) error {
	if fm.store == nil {
		return nil
	}

	// Phase 0: rebuild the host registry. We do this first so VM
	// recovery can dispatch to the correct per-host provider and the
	// scheduler is immediately usable post-restart. Without this step
	// every subsequent provision would fall through to the single
	// fm.provider path and ignore registered hosts entirely.
	hostRecords, err := fm.store.ListHosts(ctx)
	if err != nil {
		return fmt.Errorf("list persisted hosts: %w", err)
	}
	for _, hr := range hostRecords {
		host := fm.hostFromRecord(hr)
		var p Provider
		if fm.hostProviderFactory != nil {
			p = fm.hostProviderFactory(host.URL, host.Token, host.Backend)
		}
		fm.mu.Lock()
		fm.hosts[host.ID] = &host
		if p != nil {
			fm.hostProviders[host.ID] = p
		}
		fm.mu.Unlock()
	}

	records, err := fm.store.ListVMs(ctx)
	if err != nil {
		return fmt.Errorf("list persisted vms: %w", err)
	}

	recovered := make(map[string]*vm, len(records))
	for _, record := range records {
		recoveredVM := &vm{
			id:                 record.ID,
			state:              record.State,
			taskID:             record.TaskID,
			hostID:             record.HostID,
			url:                record.URL,
			spec:               record.Spec,
			authTokenEncrypted: record.AuthTokenEncrypted,
			secretsEncrypted:   record.SecretsEncrypted,
			// All recovered VMs run the fused profile today, so the drain
			// command is reconstructed from the single profile rather than
			// persisted as a DB column.
			drainCommand: DefaultFusedDrainCommand,
			endpoints:    record.Endpoints,
			createdAt:    record.CreatedAt,
			updatedAt:    record.UpdatedAt,
			// activity is not persisted, so recovery restarts the idle
			// window rather than inheriting a stale one.
			lastActivityAt: time.Now(),
			err:            record.LastError,
		}

		if recoveredVM.state == VMStateRunning {
			if err := fm.reattachVMFromProvider(ctx, recoveredVM); err != nil {
				recoveredVM.state = VMStateDestroying
				recoveredVM.taskID = ""
				recoveredVM.err = "vm missing from provider during recovery"
				recoveredVM.updatedAt = time.Now()
			}
		} else if recoveredVM.state == VMStateProvisioning {
			// Provisioning was interrupted by a crash. The sandbox may
			// already exist in the provider (a crash between Create and
			// finalize leaves it live), so re-attach via provider.Get and
			// promote the VM to Running instead of destroying it. This
			// preserves the in-flight work — and the uploadfiles/retry that
			// would otherwise 404 against a destroyed sandbox — and keeps
			// its task assigned (the task-recovery pass below retains tasks
			// whose VM recovered to Running). Only if the VM never materialised
			// in the provider do we fall back to the pre-existing teardown so
			// the half-built record is cleaned up and the task retried (#68).
			if err := fm.reattachVMFromProvider(ctx, recoveredVM); err != nil {
				recoveredVM.state = VMStateDestroying
				recoveredVM.taskID = ""
				recoveredVM.err = "provisioning interrupted by orchestrator crash"
				recoveredVM.updatedAt = time.Now()
			} else {
				recoveredVM.state = VMStateRunning
				recoveredVM.updatedAt = time.Now()
			}
		}

		recovered[recoveredVM.id] = recoveredVM
	}

	fm.mu.Lock()
	for id, recoveredVM := range recovered {
		fm.vms[id] = recoveredVM
	}
	fm.mu.Unlock()

	// Heal any drift in the persisted host Allocated counters by re-deriving
	// them from the live VM bindings. This must run after the Destroying
	// demotions above and after fm.vms is populated, so demoted VMs are
	// excluded from the sums (#39).
	recomputedHosts := fm.recomputeHostAllocations()

	// Persist the healed host counters so the durable view matches memory.
	for _, h := range recomputedHosts {
		fm.persistHostRecordBackground(fm.hostToRecord(h))
	}

	for id, recoveredVM := range recovered {
		if err := fm.persistVM(ctx, recoveredVM); err != nil {
			fm.logger.Warn("persist recovered vm failed", "vm", id, "err", err)
		}
		if recoveredVM.taskID != "" {
			if err := fm.store.UpsertTask(ctx, TaskRecord{
				TaskID:     recoveredVM.taskID,
				VMID:       recoveredVM.id,
				RunStatus:  TaskRunRunning,
				RetryCount: 0,
				LastError:  "",
				AssignedAt: recoveredVM.createdAt,
				UpdatedAt:  time.Now(),
			}); err != nil {
				fm.logger.Warn("persist recovered task failed", "task", recoveredVM.taskID, "vm", recoveredVM.id, "err", err)
			}
		}
		fm.appendEvent(ctx, "vm", id, "vm.recovered", map[string]any{"state": recoveredVM.state})

		// Re-upload secrets to recovered, still-running VMs. Without
		// this step, a guest whose tmpfs survived the orchestrator
		// crash (which it generally does — the orchestrator running
		// is independent of the guest's own lifetime) would still
		// have its old copy at the guest agent's secrets path, but a
		// guest that *also* rebooted in the meantime would come up
		// with no secrets. Re-uploading is cheap and idempotent so we
		// always do it.
		if recoveredVM.state == VMStateRunning && len(recoveredVM.secretsEncrypted) > 0 && recoveredVM.env != nil {
			if err := fm.reuploadSecrets(ctx, recoveredVM); err != nil {
				fm.logger.Warn("recover: re-upload secrets failed", "vm", id, "err", err)
				fm.appendEvent(ctx, "vm", id, "secrets.recovery_failed", map[string]any{"error": err.Error()})
			} else {
				fm.appendEvent(ctx, "vm", id, "secrets.recovered", nil)
			}
		}
	}

	tasks, err := fm.store.ListTasks(ctx)
	if err != nil {
		return fmt.Errorf("list persisted tasks: %w", err)
	}
	for _, task := range tasks {
		if task.RunStatus != TaskRunAssigned && task.RunStatus != TaskRunRunning {
			continue
		}
		recoveredVM, ok := recovered[task.VMID]
		if ok && recoveredVM.state == VMStateRunning {
			continue
		}
		task.RunStatus = TaskRunFailed
		task.LastError = "vm missing during orchestrator recovery"
		task.UpdatedAt = time.Now()
		if err := fm.store.UpsertTask(ctx, task); err != nil {
			fm.logger.Warn("mark orphaned task failed", "task", task.TaskID, "vm", task.VMID, "err", err)
			continue
		}
		fm.appendEvent(ctx, "task", task.TaskID, "task.failed", map[string]any{"vm_id": task.VMID, "error": task.LastError})
	}

	return nil
}

// hasTrackedDestroyingVM reports whether any fleet-tracked VM is currently in
// the Destroying state (i.e. being torn down but not yet reaped). Used to gate
// in-cycle allocation recompute: a Destroying VM still present in the provider
// still holds its host devices, so recompute (which excludes Destroying VMs)
// must not run while one is in flight.
func (fm *FleetManager) hasTrackedDestroyingVM() bool {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for _, v := range fm.vms {
		if v.state == VMStateDestroying {
			return true
		}
	}
	return false
}

// recomputeHostAllocations re-derives every host's Allocated capacity from
// the live VMs currently tracked in fm.vms, rather than trusting the
// persisted Allocated counter, and returns a snapshot of the corrected
// hosts so the caller can persist them. Persisted counters can drift if a
// crash interleaves an allocate/deallocate with the async write-behind;
// summing the surviving specs/bindings heals that drift (#39).
//
// VMs in the Destroying state (missing from the provider, or interrupted
// mid provision) have already released their resources conceptually, so
// they are excluded from the sums. Callers must have populated fm.vms and
// applied any Destroying demotions before calling.
func (fm *FleetManager) recomputeHostAllocations() []Host {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	// group live VMs by host so each host is summed in a single pass.
	live := make(map[string][]*vm, len(fm.hosts))
	for _, v := range fm.vms {
		if v.state == VMStateDestroying {
			continue
		}
		live[v.hostID] = append(live[v.hostID], v)
	}

	recomputed := make([]Host, 0, len(fm.hosts))
	for _, h := range fm.hosts {
		var alloc HostCapacity
		var boundDeviceUUIDs []string
		var boundMIGUUIDs []string
		var migCounts map[string]int
		perInstanceMIG := len(h.Capacity.MIGInstances) > 0
		perDevice := len(h.Capacity.GPUDevices) > 0
		for _, v := range live[h.ID] {
			alloc.CPUs += v.spec.CPUs
			alloc.RamMB += v.spec.RamMB
			alloc.StorageGB += v.spec.StorageGB
			alloc.VMCount++
			switch {
			case v.spec.GPUProfile != "":
				// MIG VM. a per-instance host binds concrete uuids onto
				// v.spec.MIGInstanceUUIDs; a legacy count-map host only carries
				// the profile + count. both are reconciled below from the
				// durable per-VM binding, so neither trusts a stale counter.
				if len(v.spec.MIGInstanceUUIDs) > 0 {
					boundMIGUUIDs = append(boundMIGUUIDs, v.spec.MIGInstanceUUIDs...)
				} else {
					if migCounts == nil {
						migCounts = make(map[string]int)
					}
					migCounts[strings.ToLower(v.spec.GPUProfile)] += int(v.spec.GPUs)
				}
			case perDevice && len(v.spec.GPUUUIDs) > 0:
				boundDeviceUUIDs = append(boundDeviceUUIDs, v.spec.GPUUUIDs...)
			default:
				alloc.GPUs += int(v.spec.GPUs)
			}
		}
		if perInstanceMIG {
			// the bound-uuid set is the source of truth on a per-instance
			// host. derive the count map from it so a stale persisted counter
			// can never drift. legacy VMs that lack bound uuids (an upgraded
			// host with pre-migration VMs) still contribute their counts.
			alloc.MIGInstanceUUIDs = boundMIGUUIDs
			alloc.MIGProfiles = migProfileCounts(boundMIGUUIDs, h.Capacity.MIGInstances)
			for profile, n := range migCounts {
				if alloc.MIGProfiles == nil {
					alloc.MIGProfiles = make(map[string]int)
				}
				alloc.MIGProfiles[profile] += n
			}
		} else if len(migCounts) > 0 {
			// legacy count-map host: the count map is the only scheduling unit.
			alloc.MIGProfiles = migCounts
		}
		if perDevice {
			// per-device host: the scalar count is the number of bound uuids.
			alloc.GPUDeviceUUIDs = boundDeviceUUIDs
			alloc.GPUs = len(boundDeviceUUIDs)
		}
		h.Allocated = alloc
		h.UpdatedAt = time.Now()
		recomputed = append(recomputed, *h)
	}
	return recomputed
}

func (fm *FleetManager) persistVM(ctx context.Context, v *vm) error {
	if fm.store == nil || v == nil {
		return nil
	}
	fm.mu.RLock()
	record := fm.vmRecordFromVM(v)
	fm.mu.RUnlock()
	return fm.store.UpsertVM(ctx, record)
}

func (fm *FleetManager) persistVMByID(ctx context.Context, vmID string) error {
	if fm.store == nil {
		return nil
	}
	record, ok := fm.snapshotVMRecord(vmID)
	if !ok {
		return nil
	}
	return fm.store.UpsertVM(ctx, record)
}

// TODO: Track background goroutines with a sync.WaitGroup so Stop() can
// wait for in-flight persists to complete before returning. Currently
// background writes can race with shutdown and lose data.
func (fm *FleetManager) persistVMBackground(vmID string) {
	go func() {
		if err := fm.persistVMByID(context.Background(), vmID); err != nil {
			fm.logger.Warn("persist vm failed", "vm", vmID, "err", err)
		}
	}()
}

func (fm *FleetManager) deleteVMBackground(vmID string) {
	if fm.store == nil {
		return
	}
	go func() {
		if err := fm.store.DeleteVM(context.Background(), vmID); err != nil {
			fm.logger.Warn("delete persisted vm failed", "vm", vmID, "err", err)
		}
	}()
}

func (fm *FleetManager) upsertTaskBackground(task TaskRecord) {
	if fm.store == nil {
		return
	}
	go func() {
		if err := fm.store.UpsertTask(context.Background(), task); err != nil {
			fm.logger.Warn("persist task failed", "task", task.TaskID, "err", err)
		}
	}()
}

// persistHostRecordBackground writes a host record snapshot to the
// state store from a goroutine. The caller is expected to have already
// snapshotted the record under fm.mu (so the persisted view is
// consistent with the in-memory state at the moment of the snapshot).
//
// Used by allocateOnHost / deallocateOnHost so capacity counters
// survive an orchestrator restart. Without it, a restart would reset
// all hosts' allocated capacity to whatever was last persisted by an
// explicit UpsertHost call (registration / cordon / uncordon), losing
// every accumulated VM placement.
func (fm *FleetManager) persistHostRecordBackground(rec HostRecord) {
	if fm.store == nil {
		return
	}
	go func() {
		if err := fm.store.UpsertHost(context.Background(), rec); err != nil {
			fm.logger.Warn("persist host failed", "host", rec.ID, "err", err)
		}
	}()
}

// AuditEvent records a security or operational event in the state
// store's event table. It is the public face of appendEvent, exposed
// so the REST middleware can log auth failures and IP rejections
// without reaching into FleetManager internals.
func (fm *FleetManager) AuditEvent(ctx context.Context, entityType, entityID, eventType string, payload map[string]any) {
	fm.appendEvent(ctx, entityType, entityID, eventType, payload)
}

func (fm *FleetManager) appendEvent(ctx context.Context, entityType, entityID, eventType string, payload map[string]any) {
	if fm.store == nil {
		return
	}
	raw := json.RawMessage(`{}`)
	if payload != nil {
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			fm.logger.Warn("marshal event payload failed", "entity_type", entityType, "entity_id", entityID, "event_type", eventType, "err", err)
			return
		}
		raw = payloadJSON
	}
	if err := fm.store.AppendEvent(ctx, EventRecord{
		EntityType: entityType,
		EntityID:   entityID,
		EventType:  eventType,
		Payload:    raw,
		CreatedAt:  time.Now(),
	}); err != nil {
		fm.logger.Warn("append event failed", "entity_type", entityType, "entity_id", entityID, "event_type", eventType, "err", err)
	}
}

func (fm *FleetManager) appendEventBackground(entityType, entityID, eventType string, payload map[string]any) {
	go fm.appendEvent(context.Background(), entityType, entityID, eventType, payload)
}

func (fm *FleetManager) snapshotVMRecord(vmID string) (VMRecord, bool) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	v, ok := fm.vms[vmID]
	if !ok {
		return VMRecord{}, false
	}
	return fm.vmRecordFromVM(v), true
}

func (fm *FleetManager) vmRecordFromVM(v *vm) VMRecord {
	vmURL := v.url
	if v.env != nil {
		vmURL = v.env.URL()
	}
	return VMRecord{
		ID:                 v.id,
		HostID:             v.hostID,
		NetworkHost:        networkHostFromURL(vmURL),
		State:              v.state,
		URL:                vmURL,
		TaskID:             v.taskID,
		Spec:               v.spec,
		AuthTokenEncrypted: v.authTokenEncrypted,
		SecretsEncrypted:   v.secretsEncrypted,
		LastError:          v.err,
		Endpoints:          v.endpoints,
		CreatedAt:          v.createdAt,
		UpdatedAt:          v.updatedAt,
	}
}

// reuploadSecrets decrypts the persisted secret blob for a recovered VM
// and writes it back to the guest agent's secrets path inside the guest.
// This is the "auto re-upload on restart" leg of the secrets-persistence
// design (see VMRecord.SecretsEncrypted).
//
// Returns an error if decryption fails (wrong key, tampered ciphertext)
// or the upload fails. Callers should treat both as recoverable
// (warn-and-continue) rather than fatal — the orchestrator can still
// serve the rest of its API; the affected VM just runs with whatever
// secrets it had on disk before the crash.
func (fm *FleetManager) reuploadSecrets(ctx context.Context, v *vm) error {
	if len(fm.tokenEncryptionKey) != 32 {
		return fmt.Errorf("token encryption key not configured")
	}
	if v.env == nil {
		return fmt.Errorf("vm %s has no environment handle", v.id)
	}
	plaintext, err := secrets.DecryptBytes(v.secretsEncrypted, fm.tokenEncryptionKey)
	if err != nil {
		return fmt.Errorf("decrypt secrets: %w", err)
	}
	if err := v.env.Upload(ctx, plaintext, FuseSecretsPath); err != nil {
		return fmt.Errorf("upload secrets: %w", err)
	}
	return nil
}

// networkHostFromURL extracts the host:port (or bare host) from a URL
// or returns the input verbatim when it's already in host:port form.
// orchestrator_vms.network_host stores this so reconcile and the API
// can address the VM without re-parsing.
func networkHostFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return raw
}
