package api

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/folsomintel/fuse/internal/fusefile"
	"github.com/folsomintel/fuse/internal/hostwire"
	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/folsomintel/fuse/internal/secrets"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
)

// ProviderFactory constructs a Provider for a registered host given
// its URL, auth token, and virtualization backend. The REST handler
// calls this during POST /v1/hosts to avoid importing provider-specific
// packages. In production a firecracker backend returns a
// firecracker.Provider and a qemu backend returns a qemu.Provider;
// tests pass a stub.
type ProviderFactory func(url, token string, backend orchestrator.HostBackend) orchestrator.Provider

// Handler is the HTTP dependency graph for the orchestrator REST API.
type Handler struct {
	// Fleet is the FleetManager instance the handlers proxy to.
	// In production this is *orchestrator.FleetManager. Tests may
	// pass any implementation compatible with the Go method set.
	Fleet *orchestrator.FleetManager

	// Resolver turns inline manifest payloads into raw bytes.
	// Defaults to InlineResolver if left nil.
	Resolver Resolver

	// NewProvider constructs a Provider for a host URL. Required for
	// host registration (POST /v1/hosts). Nil means host registration
	// is disabled and returns 501.
	NewProvider ProviderFactory

	// AuthToken is the static Bearer token required for all endpoints.
	// Empty means no auth (insecure/dev mode).
	AuthToken string

	// Version is the orchestrator build version, stamped into the Server
	// response header on every request (e.g. "fuse-orchestrator/0.4.0").
	// Empty renders as "fuse-orchestrator/dev".
	Version string

	// APIKeys is the store of revocable API keys. When non-nil it is
	// consulted by BearerAuth as a second accept path (after the master
	// token) and backs the /v1/api-keys management endpoints. Nil
	// disables API-key auth entirely.
	APIKeys APIKeyStore

	// AllowedCIDRs is a list of CIDR blocks from which requests are
	// accepted. Empty means open access.
	AllowedCIDRs []string

	// SecureCookies sets the Secure attribute on the session cookie
	// issued by POST /login. The server wires this to whether it is
	// serving TLS; it must be true in any cross-site browser deployment
	// (SameSite=None requires Secure) and should be true in production.
	SecureCookies bool

	// OnAuthFailure is called on every rejected auth attempt. The
	// server wires this to the FleetManager's event append for audit.
	// The requestID argument is the per-request correlation ID set
	// by [RequestIDMiddleware]; it is non-empty for any request that
	// passed through the standard [Handler.Router] chain.
	OnAuthFailure AuthFailureFunc

	// OnIPReject is called when a request is rejected by the CIDR
	// allowlist. Wired the same way as OnAuthFailure.
	OnIPReject IPRejectFunc

	// Metrics holds Prometheus counter/histogram vecs for HTTP RED
	// metrics. When non-nil, a recording middleware is installed on
	// the router. The three fields correspond to the outputs of
	// orchestrator.NewPrometheusMetrics.
	MetricsRequestsTotal    *prometheus.CounterVec
	MetricsRequestDuration  *prometheus.HistogramVec
	MetricsRequestsInFlight prometheus.Gauge
}

// Router returns an http.Handler serving the orchestrator REST API.
// It is safe to mount at any path prefix; routes are registered with
// absolute paths so `chi.Mount` and `http.StripPrefix` both work.
//
// Middleware order (outermost first):
//  1. Request-ID — assigns/propagates X-Request-ID so all downstream
//     middleware and audit callbacks share a correlation ID.
//  2. Metrics — records RED metrics for every request, including
//     those rejected by CIDR or auth.
//  3. CIDR allowlist — rejects before auth so blocked IPs can't even
//     probe for valid tokens.
//  4. Bearer auth — rejects unauthenticated requests.
//  5. Route handlers.
func (h *Handler) Router() (http.Handler, error) {
	r := chi.NewRouter()

	// Server header so a caller (e.g. `fuse connect`'s probe) can identify
	// this process even on a route it doesn't recognize.
	version := h.Version
	if version == "" {
		version = "dev"
	}
	serverHeader := "fuse-orchestrator/" + version
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Server", serverHeader)
			next.ServeHTTP(w, r)
		})
	})

	// Request ID (outermost — every other layer can read RequestID(ctx)).
	r.Use(RequestIDMiddleware)

	// Metrics middleware (records all requests including those
	// rejected by auth or CIDR).
	if h.MetricsRequestsTotal != nil {
		r.Use(MetricsMiddleware(
			h.MetricsRequestsTotal,
			h.MetricsRequestDuration,
			h.MetricsRequestsInFlight,
		))
	}

	// CIDR allowlist (if configured).
	if len(h.AllowedCIDRs) > 0 {
		cidrMW, err := CIDRAllowlist(h.AllowedCIDRs, h.OnIPReject)
		if err != nil {
			return nil, fmt.Errorf("parse allowed CIDRs: %w", err)
		}
		r.Use(cidrMW)
	}

	// Auth endpoints live *outside* BearerAuth so an unauthenticated
	// browser can exchange the token for a session cookie. They still
	// sit behind the CIDR allowlist above. login itself constant-time
	// compares the posted token; logout just clears the cookie. chi
	// requires all r.Use middleware before any route, so these are
	// registered as their own group rather than after the BearerAuth
	// Use call below.
	r.Group(func(pub chi.Router) {
		pub.Post("/login", h.login)
		pub.Post("/logout", h.logout)
	})

	// Everything else requires the bearer token (header or session
	// cookie). The protected routes are registered inside a group that
	// applies BearerAuth, so the auth endpoints above stay open.
	r.Group(func(priv chi.Router) {
		var keyAuth APIKeyAuthenticator
		if h.APIKeys != nil {
			keyAuth = h.APIKeys
		}
		priv.Use(BearerAuth(h.AuthToken, keyAuth, h.OnAuthFailure))
		h.register(priv)
	})

	// A URL that doesn't match any registered route is not the same
	// failure as a registered route whose resource doesn't exist (a 404
	// via writeError(CodeNotFound) elsewhere in this file) — it usually
	// means the caller has the wrong host, wrong port, or isn't talking to
	// the orchestrator at all. Give it its own code so the CLI can tell
	// the two apart instead of rendering a bare "not found".
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, CodeRouteNotFound,
			"no such route: "+r.Method+" "+r.URL.Path, nil)
	})

	return r, nil
}

// /health and /ready live in health.go and are mounted on the outer
// mux in server/main.go (alongside /metrics) so they can be probed
// without a Bearer token. They intentionally do not pass through
// [RequestIDMiddleware] — probes don't need correlation IDs.

func (h *Handler) register(r chi.Router) {
	r.Get("/v1/environments", h.listEnvironments)
	r.Post("/v1/environments", h.createEnvironment)
	r.Get("/v1/environments/{vmId}", h.getEnvironment)
	r.Get("/v1/environments/{vmId}/events", h.streamEnvironmentEvents)
	r.Post("/v1/environments/{vmId}", h.environmentAction)
	r.Delete("/v1/environments/{vmId}", h.destroyEnvironment)

	// Attach is a sub-path rather than an ?action= verb because it is a
	// protocol upgrade, not a JSON call: it has no request body and it never
	// returns a JSON response.
	r.Get("/v1/environments/{vmId}/attach", h.attachEnvironment)

	// The computer surface is a sub-path like snapshots: a JSON call with a
	// body and a JSON response, scoped to one environment. POST relays an
	// action to the guest agent; GET reports the guest display so a caller
	// can size its tool definition. See computer.go.
	r.Post("/v1/environments/{vmId}/computer", h.computerAction)
	r.Get("/v1/environments/{vmId}/computer", h.computerDisplay)

	// The live desktop stream is a protocol upgrade like attach, so it is a
	// sub-path of the computer surface rather than an ?action= verb. See
	// computerStream in computer.go.
	r.Get("/v1/environments/{vmId}/computer/stream", h.computerStream)

	r.Post("/v1/environments/{vmId}/snapshots", h.createSnapshot)
	r.Get("/v1/snapshots", h.listSnapshots)
	// A fixed sub-path rather than a filter on the collection above: this
	// answers "is there an artifact for this one key" with at most one record
	// off an indexed lookup, where the collection route loads and filters
	// every snapshot in the fleet. Chi matches the literal segment ahead of
	// {snapshotId}, and snapshot ids are provider-generated, so it cannot be
	// shadowed by a real id.
	r.Get("/v1/snapshots/resolve", h.resolveSnapshot)
	r.Get("/v1/snapshots/{snapshotId}", h.getSnapshot)
	r.Post("/v1/snapshots/{snapshotId}", h.snapshotAction)
	r.Delete("/v1/snapshots/{snapshotId}", h.deleteSnapshot)

	// API key management (master-token only; enforced per-handler).
	// Registered only when a key store is configured.
	if h.APIKeys != nil {
		r.Post("/v1/api-keys", h.createAPIKey)
		r.Get("/v1/api-keys", h.listAPIKeys)
		r.Delete("/v1/api-keys/{id}", h.revokeAPIKey)
	}

	// Host management (scheduler).
	r.Post("/v1/hosts", h.registerHost)
	r.Get("/v1/hosts", h.listHosts)
	r.Get("/v1/hosts/{hostId}", h.getHost)
	r.Post("/v1/hosts/{hostId}", h.hostAction)
	r.Delete("/v1/hosts/{hostId}", h.removeHost)
}

// resolver returns h.Resolver or the default inline resolver.
func (h *Handler) resolver() Resolver {
	if h.Resolver != nil {
		return h.Resolver
	}
	return InlineResolver{}
}

// ── Environment handlers ──────────────────────────────────────────

// createEnvironment provisions a new environment for a task.
//
//	@Summary		Create environment
//	@Description	Creates a VM, uploads the manifest, starts the guest agent, and assigns the task.
//	@Tags			environments
//	@Accept			json
//	@Produce		json
//	@Param			body	body		CreateEnvironmentRequest	true	"Environment spec"
//	@Success		201		{object}	Environment
//	@Failure		400		{object}	Error
//	@Failure		404		{object}	Error
//	@Failure		409		{object}	Error
//	@Failure		500		{object}	Error
//	@Failure		503		{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/environments [post]
func (h *Handler) createEnvironment(w http.ResponseWriter, r *http.Request) {
	var req CreateEnvironmentRequest
	if !decodeJSON(w, r, MaxEnvironmentBodyBytes, &req) {
		return
	}
	if req.TaskID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "task_id is required", nil)
		return
	}
	if err := validateGPUSpec(req.Spec); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}
	if err := validatePlacementSpec(req.Spec); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}
	// An unknown pin is a caller mistake, not a capacity shortfall: fail it
	// here as a 404 so no VM row is ever created for a host that cannot exist.
	if req.Spec.HostID != "" {
		if _, ok := h.Fleet.GetHost(req.Spec.HostID); !ok {
			writeError(w, http.StatusNotFound, CodeNotFound,
				fmt.Sprintf("spec.host_id: host %q is not registered", req.Spec.HostID), nil)
			return
		}
	}
	if err := validateTimeoutSpec(req.Spec); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}
	if err := validateHealthcheck(req.Healthcheck); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}
	if err := validateDesktop(req.Desktop); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}
	// A negative bound would reach the fleet as "unset" and quietly restore
	// the default, so it is refused here where the sign is still visible.
	// The upper bound is the fleet's to enforce, since it owns the ceiling.
	if req.StartupScriptTimeoutSeconds < 0 {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"startup_script_timeout_seconds must not be negative", nil)
		return
	}

	files, err := decodeCreateFiles(req.Files)
	if err != nil {
		status, code := http.StatusBadRequest, CodeInvalidArgument
		if errors.Is(err, errCreateFilesTooLarge) {
			status, code = http.StatusRequestEntityTooLarge, CodePayloadTooLarge
		}
		writeError(w, status, code, err.Error(), nil)
		return
	}

	manifest, err := h.resolver().Resolve(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}

	if err := secrets.ValidateSecrets(manifest, req.Secrets); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}

	spec := toOrchestratorSpec(req.Spec)
	if req.SeedSnapshotID != "" {
		if req.Spec.Image != "" {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument,
				"seed_snapshot_id and spec.image are mutually exclusive: both name the rootfs to boot", nil)
			return
		}
		// Resolve the seed before provisioning so an unusable one fails the
		// request outright instead of leaving a vm to fail at boot. The host
		// pin comes from the record because the artifact is host-local.
		record, err := h.Fleet.GetSnapshotByID(r.Context(), req.SeedSnapshotID)
		if err != nil {
			writeFleetError(w, err)
			return
		}
		if record.State != orchestrator.SnapshotStateReady {
			writeError(w, http.StatusConflict, CodeConflict,
				fmt.Sprintf("snapshot %s is in state %s, want ready", record.SnapshotID, record.State), nil)
			return
		}
		if record.HostID == "" {
			writeError(w, http.StatusConflict, CodeConflict,
				fmt.Sprintf("snapshot %s has no recorded host, so it cannot seed an environment", record.SnapshotID), nil)
			return
		}
		spec.SeedSnapshotID = record.SnapshotID
		spec.PinnedHostID = record.HostID
	}
	info, err := h.Fleet.ProvisionAndAssign(r.Context(), req.TaskID, spec, manifest, req.Secrets, orchestrator.BootOptions{
		StartupScript:        req.StartupScript,
		StartupScriptTimeout: time.Duration(req.StartupScriptTimeoutSeconds) * time.Second,
		GatewayURL:           req.GatewayURL,
		GatewayToken:         req.GatewayToken,
		Expose:               toOrchestratorExpose(req.Expose),
		Healthcheck:          toOrchestratorHealthcheck(req.Healthcheck),
		Desktop:              toOrchestratorDesktop(req.Desktop),

		Files: files,
	})
	if err != nil {
		writeFleetErrorRedacted(w, err, req.Secrets)
		return
	}

	writeJSON(w, http.StatusCreated, toAPIEnvironment(*info))
}

// errCreateFilesTooLarge marks the one decodeCreateFiles failure that is a
// size problem rather than a shape problem, so createEnvironment can answer
// 413 for it and 400 for everything else. A caller that overshot the cap needs
// to know it sent something well-formed and simply too big.
var errCreateFilesTooLarge = errors.New("files: too large")

// decodeCreateFiles turns CreateEnvironmentRequest.Files (guest path ->
// base64) into the byte map BootOptions carries, rejecting anything that
// cannot safely be written into a guest.
//
// The Fusefile compiler applies the same rules client-side, but it runs on the
// caller's machine and is trivially bypassed, and unlike a bad spec field a bad
// path here would overwrite something: /fuse holds the guest agent's manifest,
// its resolved secrets, and its TLS credentials. The profile merges its own
// files last so it always wins a collision (see FusedAgentSpec); this check is
// the half of that guarantee that fails loudly instead of silently.
//
// Paths are validated in sorted order so a request with several bad ones always
// reports the same first offender.
func decodeCreateFiles(files map[string]string) (map[string][]byte, error) {
	if len(files) == 0 {
		return nil, nil
	}

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := make(map[string][]byte, len(files))
	var total int
	for _, p := range paths {
		switch {
		case !path.IsAbs(p):
			return nil, fmt.Errorf("files: %q must be an absolute guest path", p)
		case strings.ContainsAny(p, "\x00\n"):
			return nil, fmt.Errorf("files: %q must not contain newlines or NUL bytes", p)
		case p != path.Clean(p):
			return nil, fmt.Errorf("files: %q is not a clean path (want %q)", p, path.Clean(p))
		case p == orchestrator.ReservedGuestDir || strings.HasPrefix(p, orchestrator.ReservedGuestDir+"/"):
			return nil, fmt.Errorf("files: %q is under %s, which is reserved for the guest agent's own files",
				p, orchestrator.ReservedGuestDir)
		}

		data, err := base64.StdEncoding.DecodeString(files[p])
		if err != nil {
			return nil, fmt.Errorf("files: %q content is not valid base64: %w", p, err)
		}
		total += len(data)
		if total > fusefile.MaxCopyBytes {
			return nil, fmt.Errorf("%w: files total more than the %d byte limit for one create request",
				errCreateFilesTooLarge, fusefile.MaxCopyBytes)
		}
		out[p] = data
	}
	return out, nil
}

// validateGPUSpec enforces GPU request invariants at the API boundary so
// raw SDK/API callers get the same checks the fusefile compiler applies:
// no negative counts, and a MIG profile only in mig-parted form alongside
// a positive instance count. The fusefile compiler validates too, but it
// runs client-side and is trivially bypassed.
func validateGPUSpec(s ResourceSpec) error {
	if s.GPUs < 0 {
		return errors.New("spec.gpus must not be negative")
	}
	if s.GPUProfile != "" {
		if !fusefile.ValidGPUProfile(s.GPUProfile) {
			return fmt.Errorf(
				"spec.gpu_profile: invalid MIG profile %q (expected mig-parted form like \"1g.10gb\")",
				s.GPUProfile)
		}
		if s.GPUs == 0 {
			return errors.New("spec.gpu_profile requires spec.gpus >= 1 (the count of MIG instances)")
		}
		if !fusefile.KindSupportsMIG(s.GPUKind) {
			return fmt.Errorf("spec.gpu_profile %q is not valid for gpu_kind %q (that gpu does not support MIG)", s.GPUProfile, s.GPUKind)
		}
	}
	return nil
}

// validateHealthcheck enforces the environment-level probe's invariants at the
// API boundary so raw SDK callers are held to the same rules as Fusefile
// authors. The fusefile compiler validates too, but it runs client-side and is
// trivially bypassed.
//
// A nil healthcheck is the common case and always valid: the block is optional
// and its absence means the environment declares no probe.
func validateHealthcheck(hc *HealthcheckSpec) error {
	if hc == nil {
		return nil
	}
	switch {
	case hc.HTTP != nil && len(hc.Exec) > 0:
		return errors.New("healthcheck: http and exec are mutually exclusive")
	case hc.HTTP == nil && len(hc.Exec) == 0:
		return errors.New("healthcheck: http or exec is required")
	}
	if hc.HTTP != nil {
		if hc.HTTP.Port < 1 || hc.HTTP.Port > 65535 {
			return errors.New("healthcheck.http.port must be between 1 and 65535")
		}
		// the guest builds the probe URL by concatenation, so a path without a
		// leading slash would silently probe a different one.
		if p := hc.HTTP.Path; p != "" && !strings.HasPrefix(p, "/") {
			return fmt.Errorf("healthcheck.http.path must start with a slash, got %q", p)
		}
	}
	for i, arg := range hc.Exec {
		if strings.TrimSpace(arg) == "" {
			return fmt.Errorf("healthcheck.exec[%d] must not be empty", i)
		}
	}
	// zero means "omitted, use the guest agent's default" for each of these,
	// so only a negative value, which no default could stand in for, is
	// refused.
	switch {
	case hc.IntervalSeconds < 0:
		return errors.New("healthcheck.interval_seconds must not be negative")
	case hc.TimeoutSeconds < 0:
		return errors.New("healthcheck.timeout_seconds must not be negative")
	case hc.StartPeriodSeconds < 0:
		return errors.New("healthcheck.start_period_seconds must not be negative")
	case hc.Retries < 0:
		return errors.New("healthcheck.retries must not be negative")
	}
	// attempts run one at a time in the guest, so a timeout above the interval
	// stretches the interval rather than overlapping attempts. only checked
	// when the caller set both: a default filled in guest-side is never in
	// conflict with an explicit value here.
	if hc.IntervalSeconds > 0 && hc.TimeoutSeconds > hc.IntervalSeconds {
		return fmt.Errorf("healthcheck.timeout_seconds (%d) must not exceed healthcheck.interval_seconds (%d)",
			hc.TimeoutSeconds, hc.IntervalSeconds)
	}
	return nil
}

// validateDesktop enforces the desktop geometry's bounds at the API boundary
// so raw SDK callers are held to the same rules as Fusefile authors. The
// bounds mirror internal/fusefile's: 320 to 3840 on each axis, both required.
//
// A nil desktop is the common case and always valid: the block is optional
// and its absence means the environment declares no desktop.
func validateDesktop(d *DesktopSpec) error {
	if d == nil {
		return nil
	}
	if d.Width < 320 || d.Width > 3840 {
		return fmt.Errorf("desktop.width must be between 320 and 3840, got %d", d.Width)
	}
	if d.Height < 320 || d.Height > 3840 {
		return fmt.Errorf("desktop.height must be between 320 and 3840, got %d", d.Height)
	}
	return nil
}

// validatePlacementSpec enforces the placement vocabulary at the API boundary
// so raw SDK/API callers are held to the same label rules as Fusefile authors.
// Whether a pin or a selector can actually be satisfied is the scheduler's
// call; this only rejects labels that could never match anything.
func validatePlacementSpec(s ResourceSpec) error {
	keys := make([]string, 0, len(s.Labels))
	for key := range s.Labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !fusefile.ValidLabel(key) {
			return fmt.Errorf("spec.labels: invalid label key %q", key)
		}
		if value := s.Labels[key]; !fusefile.ValidLabel(value) {
			return fmt.Errorf("spec.labels[%s]: invalid label value %q", key, value)
		}
	}
	return nil
}

// validateTimeoutSpec enforces the duration invariants at the API boundary,
// mirroring what the fusefile compiler applies client-side: no negative
// windows, and no sub-minute idle timeout (idle expiry is detected on the
// reconcile loop, so a shorter window promises accuracy it cannot deliver).
func validateTimeoutSpec(s ResourceSpec) error {
	if s.MaxRuntimeSeconds < 0 {
		return errors.New("spec.max_runtime_seconds must not be negative")
	}
	if s.IdleTimeoutSeconds < 0 {
		return errors.New("spec.idle_timeout_seconds must not be negative")
	}
	if s.IdleTimeoutSeconds > 0 && time.Duration(s.IdleTimeoutSeconds)*time.Second < fusefile.MinIdleTimeout {
		return fmt.Errorf("spec.idle_timeout_seconds must be at least %d (idle detection runs on the reconcile loop)",
			int64(fusefile.MinIdleTimeout.Seconds()))
	}
	return nil
}

// listEnvironments returns tracked environments with optional filters.
//
//	@Summary	List environments
//	@Tags		environments
//	@Produce	json
//	@Param		task_id	query		string	false	"Filter by task ID"
//	@Param		state	query		string	false	"Filter by state"	Enums(provisioning, running, destroying)
//	@Param		host_id	query		string	false	"Filter by host ID"
//	@Param		limit	query		int		false	"Max results per page (default 50, max 200)"
//	@Param		cursor	query		string	false	"Opaque pagination cursor from a previous response's next_cursor"
//	@Success	200		{object}	EnvironmentList
//	@Security	BearerAuth
//	@Router		/v1/environments [get]
func (h *Handler) listEnvironments(w http.ResponseWriter, r *http.Request) {
	limit, cursor := parsePagination(r)
	filter := orchestrator.VMFilter{
		TaskID: r.URL.Query().Get("task_id"),
		State:  orchestrator.VMState(r.URL.Query().Get("state")),
		HostID: r.URL.Query().Get("host_id"),
		Limit:  limit,
		Cursor: cursor,
	}

	fleet, next, err := h.Fleet.ListFleetFiltered(filter)
	if err != nil {
		writeFleetError(w, err)
		return
	}
	out := EnvironmentList{Environments: make([]Environment, 0, len(fleet))}
	for _, v := range fleet {
		out.Environments = append(out.Environments, toAPIEnvironment(v))
	}
	if next != "" {
		out.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, out)
}

// getEnvironment fetches a single environment by VM ID.
//
//	@Summary	Get environment
//	@Tags		environments
//	@Produce	json
//	@Param		vmId	path		string	true	"VM identifier"
//	@Success	200		{object}	Environment
//	@Failure	404		{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/environments/{vmId} [get]
func (h *Handler) getEnvironment(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")
	info, ok := h.Fleet.GetVM(vmID)
	if !ok {
		writeError(w, http.StatusNotFound, CodeNotFound, "vm "+vmID+" not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, toAPIEnvironment(info))
}

// destroyEnvironment tears down a VM.
//
//	@Summary		Destroy environment
//	@Description	Forcefully destroys the VM and marks its task failed.
//	@Tags			environments
//	@Param			vmId	path	string	true	"VM identifier"
//	@Success		204		"VM destroyed"
//	@Failure		404		{object}	Error
//	@Failure		500		{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/environments/{vmId} [delete]
func (h *Handler) destroyEnvironment(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")
	if err := h.Fleet.DestroyVM(r.Context(), vmID); err != nil {
		writeFleetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Snapshot handlers ─────────────────────────────────────────────

// createSnapshot checkpoints a running VM.
//
//	@Summary	Create snapshot
//	@Tags		snapshots
//	@Accept		json
//	@Produce	json
//	@Param		vmId	path		string					true	"VM identifier"
//	@Param		body	body		CreateSnapshotRequest	false	"Snapshot options"
//	@Success	201		{object}	Snapshot
//	@Failure	404		{object}	Error
//	@Failure	409		{object}	Error
//	@Failure	500		{object}	Error
//	@Failure	501		{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/environments/{vmId}/snapshots [post]
func (h *Handler) createSnapshot(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")

	var req CreateSnapshotRequest
	if !decodeOptionalJSON(w, r, MaxJSONBodyBytes, &req) {
		return
	}

	var retentionUntil *time.Time
	if req.RetentionSeconds > 0 {
		t := time.Now().Add(time.Duration(req.RetentionSeconds) * time.Second)
		retentionUntil = &t
	}
	var exports []orchestrator.SnapshotExportRecord
	if req.ExportRef != "" {
		exports = append(exports, orchestrator.SnapshotExportRecord{
			Destination: req.ExportRef,
			Status:      orchestrator.SnapshotExportStatus(req.ExportStatus),
			RequestedAt: time.Now(),
			UpdatedAt:   time.Now(),
		})
		if exports[0].Status == "" {
			exports[0].Status = orchestrator.SnapshotExportPending
		}
	}

	record, err := h.Fleet.CreateSnapshot(r.Context(), vmID, orchestrator.SnapshotOptions{
		Comment:        req.Comment,
		Mode:           orchestrator.SnapshotMode(req.Mode),
		RetentionUntil: retentionUntil,
		Metadata:       req.Metadata,
		Exports:        exports,
		LayerKey:       req.LayerKey,
		// Passed straight through. The orchestrator decides whether the
		// backend can honour it, and reports what it actually wrote back as
		// the record's Kind; nothing here echoes the request into the
		// response, because a caller reading its own flag back is exactly the
		// mistake the recorded kind exists to prevent.
		Live: req.Live,
		// Security boundary: a layer is filed under the scope the caller
		// authenticated as, never one it can name. It is read back the same way
		// by resolveSnapshot, which is what makes a write and a later lookup
		// meet at all. See callerTenantID.
		TenantID: callerTenantID(r.Context()),
	})
	if err != nil {
		writeFleetError(w, err)
		return
	}
	out := toAPISnapshot(record)
	if req.Comment != "" && out.Comment == "" {
		out.Comment = req.Comment
	}
	if req.ExportRef != "" && out.ExportRef == "" {
		out.ExportRef = req.ExportRef
	}
	writeJSON(w, http.StatusCreated, out)
}

// listSnapshots returns snapshots with optional filters.
//
//	@Summary	List snapshots
//	@Tags		snapshots
//	@Produce	json
//	@Param		vm_id		query		string	false	"Filter by VM ID"
//	@Param		task_id		query		string	false	"Filter by task ID"
//	@Param		tenant_id	query		string	false	"Filter by tenant ID"
//	@Param		state		query		string	false	"Filter by state"	Enums(creating, ready, restoring, deleting, error)
//	@Param		mode		query		string	false	"Filter by creation mode"	Enums(manual, auto, build)
//	@Param		name		query		string	false	"Filter by metadata name (build artifact lookup key)"
//	@Param		layer_key	query		string	false	"Filter by build layer cache key"
//	@Param		arch		query		string	false	"Filter by the architecture the artifact was built on"	Enums(amd64, arm64)
//	@Param		limit		query		int		false	"Max results per page (default 50, max 200)"
//	@Param		cursor		query		string	false	"Opaque pagination cursor from a previous response's next_cursor"
//	@Success	200			{object}	SnapshotList
//	@Security	BearerAuth
//	@Router		/v1/snapshots [get]
func (h *Handler) listSnapshots(w http.ResponseWriter, r *http.Request) {
	limit, cursor := parsePagination(r)
	filter := orchestrator.SnapshotFilter{
		VMID:     r.URL.Query().Get("vm_id"),
		TaskID:   r.URL.Query().Get("task_id"),
		TenantID: r.URL.Query().Get("tenant_id"),
		State:    orchestrator.SnapshotState(r.URL.Query().Get("state")),
		Mode:     orchestrator.SnapshotMode(r.URL.Query().Get("mode")),
		Name:     r.URL.Query().Get("name"),
		LayerKey: r.URL.Query().Get("layer_key"),
		// normalized so a browsing operator typing the uname spelling
		// ("x86_64") sees the same rows the scheduler's vocabulary stores.
		Arch:   orchestrator.NormalizeArch(r.URL.Query().Get("arch")),
		Limit:  limit,
		Cursor: cursor,
	}

	records, next, err := h.Fleet.ListSnapshotsFiltered(r.Context(), filter)
	if err != nil {
		writeFleetError(w, err)
		return
	}
	out := SnapshotList{Snapshots: make([]Snapshot, 0, len(records))}
	for _, s := range records {
		out.Snapshots = append(out.Snapshots, toAPISnapshot(s))
	}
	if next != "" {
		out.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, out)
}

// resolveSnapshot resolves one build layer cache key to at most one artifact.
//
//	@Summary		Resolve a build layer
//	@Description	Returns the newest ready artifact for one layer cache key, scoped to the caller's tenant. A cold cache is a 200 with found=false, not an error: a client walks its layer chain deepest-first, one key per call, and stops at the first hit.
//	@Tags			snapshots
//	@Produce		json
//	@Param			layer_key	query		string	true	"Derived cache key of the setup step"
//	@Param			arch		query		string	true	"Architecture the artifact must have been built on"	Enums(amd64, arm64)
//	@Success		200			{object}	ResolveSnapshotResponse
//	@Failure		400			{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/snapshots/resolve [get]
func (h *Handler) resolveSnapshot(w http.ResponseWriter, r *http.Request) {
	layerKey := r.URL.Query().Get("layer_key")
	if layerKey == "" {
		// An empty key matches nothing by construction, so answering "miss"
		// would be technically true and completely useless: it would hide a
		// caller that failed to derive its keys behind a cache that always
		// looks cold.
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "layer_key is required", nil)
		return
	}

	// Required, never defaulted. An artifact is a rootfs image, and a rootfs
	// does not travel across architectures, so guessing here would hand back
	// bytes the caller cannot boot, and it would do it silently, at the one
	// moment the caller believes it got a cache hit.
	arch := orchestrator.NormalizeArch(r.URL.Query().Get("arch"))
	if arch == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "arch is required", nil)
		return
	}

	// Security boundary: the tenant comes from how the caller authenticated,
	// never from the request. See callerTenantID.
	record, ok, err := h.Fleet.ResolveLayer(r.Context(), callerTenantID(r.Context()), layerKey, arch)
	if err != nil {
		writeFleetError(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, ResolveSnapshotResponse{Found: false})
		return
	}
	snapshot := toAPISnapshot(record)
	writeJSON(w, http.StatusOK, ResolveSnapshotResponse{Found: true, Snapshot: &snapshot})
}

// getSnapshot fetches a single snapshot by ID.
//
//	@Summary	Get snapshot
//	@Tags		snapshots
//	@Produce	json
//	@Param		snapshotId	path		string	true	"Snapshot identifier"
//	@Success	200			{object}	Snapshot
//	@Failure	404			{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/snapshots/{snapshotId} [get]
func (h *Handler) getSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshotID := chi.URLParam(r, "snapshotId")
	record, err := h.Fleet.GetSnapshotByID(r.Context(), snapshotID)
	if err != nil {
		writeFleetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPISnapshot(record))
}

// deleteSnapshot removes a leaf snapshot.
//
//	@Summary	Delete snapshot
//	@Tags		snapshots
//	@Param		snapshotId	path	string	true	"Snapshot identifier"
//	@Success	204			"Snapshot deleted"
//	@Failure	404			{object}	Error
//	@Failure	409			{object}	Error
//	@Failure	500			{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/snapshots/{snapshotId} [delete]
func (h *Handler) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshotID := chi.URLParam(r, "snapshotId")
	if err := h.Fleet.DeleteSnapshotByID(r.Context(), snapshotID); err != nil {
		writeFleetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Snapshot actions ──────────────────────────────────────────────

// snapshotAction dispatches an action on a snapshot via ?action= query param.
//
//	@Summary		Snapshot action
//	@Description	Perform an action on a snapshot. Currently supports: restore.
//	@Tags			snapshots
//	@Param			snapshotId	path	string	true	"Snapshot identifier"
//	@Param			action		query	string	true	"Action to perform"	Enums(restore)
//	@Success		204			"Action succeeded"
//	@Failure		400			{object}	Error
//	@Failure		404			{object}	Error
//	@Failure		409			{object}	Error
//	@Failure		500			{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/snapshots/{snapshotId} [post]
func (h *Handler) snapshotAction(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("action") {
	case "restore":
		h.restoreSnapshot(w, r)
	default:
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"missing or unknown action query parameter", nil)
	}
}

func (h *Handler) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshotID := chi.URLParam(r, "snapshotId")
	if err := h.Fleet.RestoreSnapshotByID(r.Context(), snapshotID); err != nil {
		writeFleetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Environment actions ───────────────────────────────────────────

// environmentAction dispatches an action on an environment via ?action= query param.
//
//	@Summary		Environment action
//	@Description	Perform an action on an environment. Supports: rotate-token, drain, fork, exec.
//	@Tags			environments
//	@Param			vmId	path	string	true	"VM identifier"
//	@Param			action	query	string	true	"Action to perform"	Enums(rotate-token, drain, fork, exec)
//	@Success		200		{object}	Environment	"Drain succeeded; updated VM state returned"
//	@Success		200		{object}	ExecEnvironmentResponse	"Exec ran; a non-zero exit_code is still a 200"
//	@Success		201		{object}	Environment	"Fork succeeded; new environment returned"
//	@Success		204		"Action succeeded"
//	@Failure		400		{object}	Error
//	@Failure		404		{object}	Error
//	@Failure		409		{object}	Error
//	@Failure		500		{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/environments/{vmId} [post]
func (h *Handler) environmentAction(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("action") {
	case "rotate-token":
		h.rotateToken(w, r)
	case "drain":
		h.drainEnvironment(w, r)
	case "fork":
		h.forkEnvironment(w, r)
	case "exec":
		h.execEnvironment(w, r)
	default:
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"missing or unknown action query parameter", nil)
	}
}

func (h *Handler) rotateToken(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")
	if err := h.Fleet.RotateToken(r.Context(), vmID); err != nil {
		writeFleetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// drainEnvironment is the first phase of two-phase teardown. It runs the
// configured drain command inside the guest to gracefully quiesce in-guest
// workloads (an empty command leaves the VM Draining for the caller to
// DELETE), then leaves the VM in the Draining state so the caller can inspect
// outputs before issuing DELETE. See FleetManager.Drain for the full
// state-machine contract.
//
// Unlike most environment actions this returns 200 with the updated
// Environment body (rather than 204) so the caller can observe the
// state transition without a follow-up GET.
func (h *Handler) drainEnvironment(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")
	if err := h.Fleet.Drain(r.Context(), vmID); err != nil {
		writeFleetError(w, err)
		return
	}
	info, ok := h.Fleet.GetVM(vmID)
	if !ok {
		// Drain succeeded but the VM disappeared between the call
		// and the read — extremely unlikely, but return 204 rather
		// than synthesising a stale body.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, toAPIEnvironment(info))
}

// forkEnvironment creates a new environment seeded from an existing one and
// returns it with 201. an optional body selects an existing snapshot to fork
// from (reuse_snapshot_id) and a comment; an empty body snapshots the source
// first. the new vm id is resolved via GetVM, mirroring drainEnvironment.
func (h *Handler) forkEnvironment(w http.ResponseWriter, r *http.Request) {
	vmID := chi.URLParam(r, "vmId")

	var req ForkEnvironmentRequest
	if !decodeOptionalJSON(w, r, MaxJSONBodyBytes, &req) {
		return
	}

	newID, err := h.Fleet.ForkEnvironment(r.Context(), vmID, orchestrator.ForkOptions{
		ReuseSnapshotID: req.ReuseSnapshotID,
		Comment:         req.Comment,
	})
	if err != nil {
		writeFleetError(w, err)
		return
	}
	info, ok := h.Fleet.GetVM(newID)
	if !ok {
		// fork reported success but the new vm is not readable yet; return
		// 204 rather than synthesising a stale body.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIEnvironment(info))
}

// ── Host management handlers ──────────────────────────────────────

// hostAction dispatches an action on a host via ?action= query param.
//
//	@Summary		Host action
//	@Description	Perform an action on a host. Supports: cordon (mark unschedulable), uncordon (return to scheduling). Requires the master token.
//	@Tags			hosts
//	@Param			hostId	path	string	true	"Host identifier"
//	@Param			action	query	string	true	"Action to perform"	Enums(cordon, uncordon)
//	@Success		204		"Action succeeded"
//	@Failure		400		{object}	Error
//	@Failure		404		{object}	Error
//	@Failure		500		{object}	Error
//	@Security		BearerAuth
//	@Router			/v1/hosts/{hostId} [post]
func (h *Handler) hostAction(w http.ResponseWriter, r *http.Request) {
	if !h.requireMasterToken(w, r, "host mutations require the master token") {
		return
	}
	switch r.URL.Query().Get("action") {
	case "cordon":
		h.cordonHost(w, r)
	case "uncordon":
		h.uncordonHost(w, r)
	default:
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"missing or unknown action query parameter", nil)
	}
}

// registerHost registers or updates a host in the scheduler. cpus/ram_mb/
// storage_gb left at 0 are sourced from the host agent's capacity probe
// (GET /v1/capacity) when the provider supports it; a declared value always
// overrides the probe, with a warning if it exceeds what was probed.
// vm_count is scheduling policy and is never probed.
//
//	@Summary	Register host
//	@Description	Registers or updates a host. Requires the master token.
//	@Tags		hosts
//	@Accept		json
//	@Produce	json
//	@Param		body	body		RegisterHostRequest	true	"Host registration"
//	@Success	201		{object}	HostInfo
//	@Failure	400		{object}	Error
//	@Failure	500		{object}	Error
//	@Failure	502		{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/hosts [post]
func (h *Handler) registerHost(w http.ResponseWriter, r *http.Request) {
	// Registration takes a caller-supplied URL and token and can replace the
	// provider details of an existing host id, so it decides where the
	// orchestrator sends every subsequent VM operation. API keys carry no
	// scopes, so anything short of the master token would make every issued
	// key able to redirect the control plane.
	if !h.requireMasterToken(w, r, "host registration requires the master token") {
		return
	}
	if h.NewProvider == nil {
		writeError(w, http.StatusNotImplemented, CodeInternal,
			"host registration is disabled (no provider factory configured)", nil)
		return
	}

	var req RegisterHostRequest
	if !decodeJSON(w, r, MaxJSONBodyBytes, &req) {
		return
	}
	if req.ID == "" || req.URL == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "id and url are required", nil)
		return
	}
	// Validate the destination before anything is constructed from it, so a
	// rejected URL is never dialled, probed, or persisted.
	if err := hostwire.ValidateAgentURL(req.URL); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, err.Error(), nil)
		return
	}

	backend := orchestrator.HostBackend(req.Backend)
	if backend == "" {
		backend = orchestrator.BackendFirecracker
	}
	if backend != orchestrator.BackendFirecracker && backend != orchestrator.BackendQEMU {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"backend must be \"firecracker\" or \"qemu\"", nil)
		return
	}
	if req.Capacity.GPUs > 0 && backend != orchestrator.BackendQEMU {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"gpus > 0 requires backend \"qemu\"", nil)
		return
	}
	if req.Capacity.GPUs < 0 {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "gpus must not be negative", nil)
		return
	}
	// Arch is normalized to GOARCH vocabulary; empty means "probe it" (or
	// legacy amd64 when the agent doesn't report one either).
	declaredArch := orchestrator.NormalizeArch(req.Capacity.Arch)
	if declaredArch != "" && declaredArch != orchestrator.ArchAMD64 && declaredArch != orchestrator.ArchARM64 {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			fmt.Sprintf("arch must be \"amd64\" or \"arm64\" (got %q)", req.Capacity.Arch), nil)
		return
	}
	if len(req.Capacity.MIGProfiles) > 0 && backend != orchestrator.BackendQEMU {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"mig_profiles requires backend \"qemu\"", nil)
		return
	}
	// Labels are declared, never probed, so the only check possible here is
	// the vocabulary one: reject anything that could not be written as a
	// placement selector in a Fusefile.
	for key, value := range req.Labels {
		if !fusefile.ValidLabel(key) {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument,
				fmt.Sprintf("labels: invalid label key %q", key), nil)
			return
		}
		if !fusefile.ValidLabel(value) {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument,
				fmt.Sprintf("labels[%s]: invalid label value %q", key, value), nil)
			return
		}
	}
	for profile, count := range req.Capacity.MIGProfiles {
		if !fusefile.ValidGPUProfile(profile) {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument,
				fmt.Sprintf("mig_profiles: invalid MIG profile %q (expected mig-parted form like \"1g.10gb\")", profile), nil)
			return
		}
		if count <= 0 {
			writeError(w, http.StatusBadRequest, CodeInvalidArgument,
				fmt.Sprintf("mig_profiles[%s]: count must be positive", profile), nil)
			return
		}
	}

	provider := h.NewProvider(req.URL, req.Token, backend)

	// CPUs/RamMB/StorageGB are hardware facts: a zero/negative value here
	// means the operator left it unset (the CLI's --cpus etc. default to 0),
	// so probe the host agent for the real numbers when it supports
	// CapacityProber. VMCount (and GPUs) are scheduling policy, not
	// something a host can report on its own, so they are never probed and
	// must always be declared explicitly.
	capacity := orchestrator.HostCapacity{
		CPUs:      req.Capacity.CPUs,
		RamMB:     req.Capacity.RamMB,
		StorageGB: req.Capacity.StorageGB,
		VMCount:   req.Capacity.VMCount,
		Arch:      declaredArch,
		GPUs:      req.Capacity.GPUs,
		GPUKind:   req.Capacity.GPUKind,
	}
	if len(req.Capacity.MIGProfiles) > 0 {
		// Lowercase profile keys so scheduling matches the (also
		// lowercased) spec.gpu_profile regardless of declared casing.
		capacity.MIGProfiles = make(map[string]int, len(req.Capacity.MIGProfiles))
		for profile, count := range req.Capacity.MIGProfiles {
			capacity.MIGProfiles[strings.ToLower(profile)] = count
		}
	}

	// The capacity probe doubles as an auth probe: it is the first (and,
	// at registration time, only) call the orchestrator makes to the host
	// agent with the supplied token. A 401 here means the token is wrong,
	// which is unambiguous and never a false positive, so we refuse the
	// registration outright regardless of whether capacity was declared --
	// a host the orchestrator cannot authenticate to is useless, and
	// catching it here turns a later "firecracker create vm: http 401"
	// into a clear, layer-named error. Other probe failures (agent
	// unreachable) only block when capacity was not fully declared; with
	// declared capacity we register but warn, so pre-provisioning still
	// works without silently hiding an unreachable host.
	var warnings []string
	if prober, ok := provider.(orchestrator.CapacityProber); ok {
		probed, err := prober.Capacity(r.Context())
		switch {
		case err == nil:
			capacity.CPUs, warnings = resolveCapacityField("cpus", capacity.CPUs, probed.CPUs, warnings)
			capacity.RamMB, warnings = resolveCapacityField("ram_mb", capacity.RamMB, probed.RamMB, warnings)
			capacity.StorageGB, warnings = resolveCapacityField("storage_gb", capacity.StorageGB, probed.StorageGB, warnings)
			// GPUs are probed too: zero declared means "not set", so the
			// probed count wins. A qemu host may legitimately have none.
			capacity.GPUs, warnings = resolveCapacityField("gpus", capacity.GPUs, probed.GPUs, warnings)
			capacity.GPUKind, warnings = resolveCapacityKind(capacity.GPUKind, probed.GPUKind, warnings)
			// arch: probed fills an empty declaration, but a contradiction
			// is a hard error rather than a warning. gpu_kind mismatches are
			// labeling disputes; an arch mismatch means every image the
			// scheduler sends here would be unbootable.
			probedArch := orchestrator.NormalizeArch(probed.Arch)
			if capacity.Arch == "" {
				capacity.Arch = probedArch
			} else if probedArch != "" && probedArch != capacity.Arch {
				_ = provider.Close()
				writeError(w, http.StatusBadRequest, CodeInvalidArgument,
					fmt.Sprintf("declared arch %q contradicts the host agent's probed arch %q", capacity.Arch, probedArch), nil)
				return
			}
			// the probe is the source of truth for the per-device list.
			capacity.GPUDevices = probed.GPUDevices
			// per-instance MIG inventory is probed too. when the agent reports
			// instances, the scheduler binds specific uuids to VMs; the count
			// map (MIGProfiles) is then a derived summary. an operator's
			// explicit --mig-profile declaration wins over the probe for the
			// count map (it is a capacity override), but the per-instance
			// inventory still rides through so allocation can bind concrete
			// instances. a host that reports neither falls back to the
			// declared count-map path unchanged.
			capacity.MIGInstances = probed.MIGInstances
			if len(probed.MIGInstances) > 0 && len(capacity.MIGProfiles) == 0 {
				capacity.MIGProfiles = make(map[string]int, len(probed.MIGInstances))
				for _, inst := range probed.MIGInstances {
					capacity.MIGProfiles[strings.ToLower(inst.Profile)]++
				}
			}
		case isAgentUnauthorized(err):
			_ = provider.Close()
			writeError(w, http.StatusBadGateway, CodeUnauthorized,
				fmt.Sprintf("host agent at %s rejected the token. this must be its FC_AGENT_TOKEN, not the orchestrator token", req.URL), nil)
			return
		case capacity.CPUs <= 0 || capacity.RamMB <= 0 || capacity.StorageGB <= 0:
			_ = provider.Close()
			writeError(w, http.StatusBadGateway, CodeInternal,
				"could not probe host capacity and cpus/ram_mb/storage_gb were not all declared explicitly: "+err.Error(), nil)
			return
		default:
			warnings = append(warnings,
				fmt.Sprintf("could not verify host agent at %s (%s); registering with declared capacity", req.URL, err.Error()))
		}
	}

	if capacity.CPUs <= 0 || capacity.RamMB <= 0 || capacity.StorageGB <= 0 || capacity.VMCount <= 0 {
		_ = provider.Close()
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"cpus, ram_mb, storage_gb, and vm_count must be positive (declare them explicitly, or omit cpus/ram_mb/storage_gb to probe a host agent that supports it)", nil)
		return
	}

	host := orchestrator.Host{
		ID:       req.ID,
		URL:      req.URL,
		Token:    req.Token,
		Region:   req.Region,
		Backend:  backend,
		Labels:   copyLabels(req.Labels),
		Capacity: capacity,
	}

	if err := h.Fleet.RegisterHost(r.Context(), host, provider); err != nil {
		writeFleetError(w, err)
		return
	}

	info, ok := h.Fleet.GetHost(req.ID)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternal, "host registered but not found", nil)
		return
	}
	resp := toAPIHost(info)
	resp.Warnings = warnings
	writeJSON(w, http.StatusCreated, resp)
}

// resolveCapacityField applies the declared-overrides-probed rule for a
// single capacity field: declared <= 0 means "not set", so the probed value
// wins outright. A positive declared value that exceeds what was probed is
// kept (it's the operator's explicit call, e.g. a deliberate overcommit) but
// appends a warning so it doesn't pass silently.
func resolveCapacityField(name string, declared, probed int, warnings []string) (int, []string) {
	if declared <= 0 {
		return probed, warnings
	}
	if probed > 0 && declared > probed {
		warnings = append(warnings, fmt.Sprintf("declared %s (%d) exceeds probed host capacity (%d)", name, declared, probed))
	}
	return declared, warnings
}

// resolveCapacityKind is the string sibling of resolveCapacityField for the
// gpu_kind label: an empty declared value means "not set", so the probed
// kind wins. When both are set and differ we keep the operator's declared
// value but append a warning so the mismatch doesn't pass silently.
func resolveCapacityKind(declared, probed string, warnings []string) (string, []string) {
	if declared == "" {
		return probed, warnings
	}
	if probed != "" && declared != probed {
		warnings = append(warnings, fmt.Sprintf("declared gpu_kind (%q) differs from probed host gpu_kind (%q)", declared, probed))
	}
	return declared, warnings
}

// isAgentUnauthorized reports whether err is a host-agent response that
// rejected the orchestrator's bearer token (HTTP 401). Providers wrap raw
// host-agent HTTP failures in orchestrator.HTTPStatusError, so a 401 is
// distinguishable from an unreachable agent or any other failure.
func isAgentUnauthorized(err error) bool {
	var httpErr *orchestrator.HTTPStatusError
	return errors.As(err, &httpErr) && httpErr.Code == http.StatusUnauthorized
}

// listHosts returns all registered hosts.
//
// Reads are deliberately available to any authenticated caller, master token
// or API key: the response carries fleet topology and capacity but never a
// host's agent token (see toAPIHost), and a caller that can create
// environments already learns which hosts exist from where they land. Every
// host *mutation* is master-token only.
//
//	@Summary	List hosts
//	@Tags		hosts
//	@Produce	json
//	@Param		limit	query		int		false	"Max results per page (default 50, max 200)"
//	@Param		cursor	query		string	false	"Opaque pagination cursor from a previous response's next_cursor"
//	@Success	200		{object}	HostList
//	@Security	BearerAuth
//	@Router		/v1/hosts [get]
func (h *Handler) listHosts(w http.ResponseWriter, r *http.Request) {
	limit, cursor := parsePagination(r)
	hosts, next, err := h.Fleet.ListHostsFiltered(orchestrator.HostFilter{Limit: limit, Cursor: cursor})
	if err != nil {
		writeFleetError(w, err)
		return
	}
	out := HostList{Hosts: make([]HostInfo, 0, len(hosts))}
	for _, host := range hosts {
		out.Hosts = append(out.Hosts, toAPIHost(host))
	}
	if next != "" {
		out.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, out)
}

// getHost fetches a single host by ID. Readable by any authenticated caller,
// for the same reason as listHosts.
//
//	@Summary	Get host
//	@Tags		hosts
//	@Produce	json
//	@Param		hostId	path		string	true	"Host identifier"
//	@Success	200		{object}	HostInfo
//	@Failure	404		{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/hosts/{hostId} [get]
func (h *Handler) getHost(w http.ResponseWriter, r *http.Request) {
	hostID := chi.URLParam(r, "hostId")
	host, ok := h.Fleet.GetHost(hostID)
	if !ok {
		writeError(w, http.StatusNotFound, CodeNotFound, "host "+hostID+" not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, toAPIHost(host))
}

func (h *Handler) cordonHost(w http.ResponseWriter, r *http.Request) {
	hostID := chi.URLParam(r, "hostId")
	if err := h.Fleet.CordonHost(r.Context(), hostID); err != nil {
		writeFleetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) uncordonHost(w http.ResponseWriter, r *http.Request) {
	hostID := chi.URLParam(r, "hostId")
	if err := h.Fleet.UncordonHost(r.Context(), hostID); err != nil {
		writeFleetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeHost removes a host from the scheduler.
//
//	@Summary	Remove host
//	@Description	Removes a host from the scheduler. Requires the master token.
//	@Tags		hosts
//	@Param		hostId	path	string	true	"Host identifier"
//	@Success	204		"Host removed"
//	@Failure	403		{object}	Error
//	@Failure	404		{object}	Error
//	@Failure	409		{object}	Error
//	@Failure	500		{object}	Error
//	@Security	BearerAuth
//	@Router		/v1/hosts/{hostId} [delete]
func (h *Handler) removeHost(w http.ResponseWriter, r *http.Request) {
	if !h.requireMasterToken(w, r, "host removal requires the master token") {
		return
	}
	hostID := chi.URLParam(r, "hostId")
	if err := h.Fleet.RemoveHost(r.Context(), hostID); err != nil {
		writeFleetError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
