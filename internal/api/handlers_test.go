package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// TODO: Add test cases for invalid/edge-case inputs: negative CPU counts,
// empty task IDs on non-create endpoints, malformed JSON bodies, oversized
// payloads. Current tests cover happy paths but not input validation boundaries.

// TODO: Add tests for metrics middleware — verify that route labels use chi
// patterns (not raw URLs) and that status codes are recorded correctly.

// ── Test doubles ──────────────────────────────────────────────────
//
// These mirror the fixtures in fleet_test.go but live in the api
// package so the handler layer can be tested in isolation. They are
// intentionally minimal — any behavior the tests don't exercise is
// stubbed to a happy-path return.

type fakeEnv struct {
	name string
	url  string

	mu          sync.Mutex
	checkpoints []orchestrator.Checkpoint
	execCalls   [][]string

	// execResult and execErr program what Exec returns, so a test can drive
	// both a clean run and a guest command that exits non-zero — which are
	// different outcomes the handler must not conflate.
	execResult orchestrator.ExecResult
	execErr    error

	// blockExec makes ExecStream run until its context is done.
	blockExec bool

	// execArgv and execOpts record what Exec was actually asked to run.
	execArgv [][]string
	execOpts []orchestrator.ExecOptions

	// execDelay makes Exec block before returning, to stand in for a
	// long-running guest command. Read without the lock so it must be set
	// before the call.
	execDelay time.Duration

	// attachStream is handed back by Attach; attachErr overrides it.
	// attachSpec records what the handler asked for.
	attachStream io.ReadWriteCloser
	attachErr    error
	attachSpec   orchestrator.AttachSpec
	attachCalls  int
}

func (e *fakeEnv) Name() string  { return e.name }
func (e *fakeEnv) URL() string   { return e.url }
func (e *fakeEnv) Token() string { return "" }

func (e *fakeEnv) Exec(_ context.Context, cmd []string, opts orchestrator.ExecOptions) (orchestrator.ExecResult, error) {
	e.mu.Lock()
	e.execArgv = append(e.execArgv, cmd)
	e.execOpts = append(e.execOpts, opts)
	res, err, delay := e.execResult, e.execErr, e.execDelay
	e.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	return res, err
}

func (e *fakeEnv) Attach(_ context.Context, spec orchestrator.AttachSpec) (io.ReadWriteCloser, error) {
	e.mu.Lock()
	e.attachSpec = spec
	e.attachCalls++
	stream, err := e.attachStream, e.attachErr
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return stream, nil
}

func (e *fakeEnv) ExecStream(ctx context.Context, _, _ io.Writer, name string, args ...string) error {
	e.mu.Lock()
	e.execCalls = append(e.execCalls, append([]string{name}, args...))
	blocking := e.blockExec
	e.mu.Unlock()
	if blocking {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// execInvocations returns a copy of the argv recorded by Exec (as opposed to
// execCommands, which records the ExecStream calls drain makes).
func (e *fakeEnv) execInvocations() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]string, len(e.execArgv))
	copy(out, e.execArgv)
	return out
}

// execCommands returns a copy of the recorded ExecStream calls so drain tests
// can assert the configured DrainCommand actually ran.
func (e *fakeEnv) execCommands() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]string, len(e.execCalls))
	copy(out, e.execCalls)
	return out
}

func (e *fakeEnv) Upload(context.Context, []byte, string) error { return nil }
func (e *fakeEnv) StartAgent(context.Context, orchestrator.AgentSpec) error {
	return nil
}
func (e *fakeEnv) Checkpoint(context.Context, string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := fmt.Sprintf("cp-%d", len(e.checkpoints)+1)
	e.checkpoints = append(e.checkpoints, orchestrator.Checkpoint{
		ID:        id,
		SizeBytes: int64(128*len(e.checkpoints) + 128),
	})
	return id, nil
}
func (e *fakeEnv) Restore(_ context.Context, checkpointID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, checkpoint := range e.checkpoints {
		if checkpoint.ID == checkpointID {
			return nil
		}
	}
	return fmt.Errorf("checkpoint %s not found", checkpointID)
}
func (e *fakeEnv) DeleteCheckpoint(_ context.Context, checkpointID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, checkpoint := range e.checkpoints {
		if checkpoint.ID != checkpointID {
			continue
		}
		e.checkpoints = append(e.checkpoints[:i], e.checkpoints[i+1:]...)
		return nil
	}
	return fmt.Errorf("checkpoint %s not found", checkpointID)
}
func (e *fakeEnv) ListCheckpoints(context.Context) ([]orchestrator.Checkpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]orchestrator.Checkpoint, len(e.checkpoints))
	copy(out, e.checkpoints)
	return out, nil
}

type fakeProvider struct {
	mu   sync.Mutex
	envs map[string]*fakeEnv

	// blockExec is stamped onto every env Create returns, modelling a
	// startup script that never terminates.
	blockExec bool
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{envs: make(map[string]*fakeEnv)}
}

func (p *fakeProvider) Create(_ context.Context, spec orchestrator.Spec) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := &fakeEnv{name: spec.Name, url: "http://" + spec.Name + ".test", blockExec: p.blockExec}
	p.envs[spec.Name] = e
	return e, nil
}

func (p *fakeProvider) Get(_ context.Context, name string) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.envs[name]
	if !ok {
		return nil, fmt.Errorf("not found: %s", name)
	}
	return e, nil
}

func (p *fakeProvider) Destroy(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.envs, name)
	return nil
}

func (p *fakeProvider) List(_ context.Context, _ string) ([]orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]orchestrator.Environment, 0, len(p.envs))
	for _, e := range p.envs {
		out = append(out, e)
	}
	return out, nil
}

func (*fakeProvider) Close() error { return nil }

// liveFakeEnv is a fakeEnv that also implements
// orchestrator.LiveSnapshotCapable, standing in for a firecracker host whose
// agent can capture guest memory. It has to be a distinct type rather than a
// flag on fakeEnv because the orchestrator decides by type assertion, so a
// bare fakeEnv is exactly the "disk snapshots only" backend the 501 path
// needs.
type liveFakeEnv struct {
	*fakeEnv
}

func (e *liveFakeEnv) CheckpointLive(_ context.Context, comment string) (orchestrator.Checkpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := orchestrator.Checkpoint{
		ID:        fmt.Sprintf("cp-%d", len(e.checkpoints)+1),
		Comment:   comment,
		SizeBytes: int64(4096*len(e.checkpoints) + 4096),
		Kind:      orchestrator.SnapshotKindLive,
	}
	e.checkpoints = append(e.checkpoints, cp)
	return cp, nil
}

// staleLiveFakeEnv reports a disk snapshot for a live request, modelling a
// host agent too old to know about the flag. It exists so the "kind is what
// was written, not what was asked for" contract is exercisable.
type staleLiveFakeEnv struct {
	*fakeEnv
}

func (e *staleLiveFakeEnv) CheckpointLive(_ context.Context, comment string) (orchestrator.Checkpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := orchestrator.Checkpoint{
		ID:        fmt.Sprintf("cp-%d", len(e.checkpoints)+1),
		Comment:   comment,
		SizeBytes: 128,
		Kind:      orchestrator.SnapshotKindDisk,
	}
	e.checkpoints = append(e.checkpoints, cp)
	return cp, nil
}

// liveFakeProvider hands out envs that satisfy LiveSnapshotCapable. stale
// swaps in the agent that answers a live request with a disk snapshot.
type liveFakeProvider struct {
	*fakeProvider
	stale bool
}

func (p *liveFakeProvider) Create(ctx context.Context, spec orchestrator.Spec) (orchestrator.Environment, error) {
	base, err := p.fakeProvider.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return p.wrap(base.(*fakeEnv)), nil
}

func (p *liveFakeProvider) Get(ctx context.Context, name string) (orchestrator.Environment, error) {
	base, err := p.fakeProvider.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return p.wrap(base.(*fakeEnv)), nil
}

func (p *liveFakeProvider) wrap(e *fakeEnv) orchestrator.Environment {
	if p.stale {
		return &staleLiveFakeEnv{fakeEnv: e}
	}
	return &liveFakeEnv{fakeEnv: e}
}

// newLiveTestHandler is newTestHandler with a provider whose envs can take
// live snapshots. stale=true makes them report disk for a live request.
func newLiveTestHandler(t *testing.T, stale bool) (*Handler, *orchestrator.FleetManager) {
	t.Helper()
	p := &liveFakeProvider{fakeProvider: newFakeProvider(), stale: stale}
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})
	return &Handler{Fleet: fm}, fm
}

// capacityProbeProvider wraps fakeProvider and implements
// orchestrator.CapacityProber so registerHost tests can exercise the
// probe-when-unset / warn-when-declared-exceeds-probed / cannot-probe paths
// deterministically.
type capacityProbeProvider struct {
	*fakeProvider
	capacity orchestrator.HostCapacity
	err      error
}

func (p *capacityProbeProvider) Capacity(_ context.Context) (orchestrator.HostCapacity, error) {
	return p.capacity, p.err
}

// ── Helpers ───────────────────────────────────────────────────────

// newTestHandler wires a real FleetManager with an in-memory state
// store against the fake provider. The returned handler is ready to
// serve requests; the test owns the mux returned by Router().
func newTestHandler(t *testing.T) (*Handler, *orchestrator.FleetManager, *fakeProvider) {
	t.Helper()
	p := newFakeProvider()
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})
	return &Handler{Fleet: fm}, fm, p
}

// encodeManifest base64-encodes a small valid-looking manifest body.
// The fake provider never actually parses it, but we send real bytes
// so the resolver path is exercised.
func encodeManifest(t *testing.T) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(`{"version":"1","services":{}}`))
}

// decodeError pulls an Error envelope out of a response body.
func decodeError(t *testing.T, body io.Reader) Error {
	t.Helper()
	var e Error
	if err := json.NewDecoder(body).Decode(&e); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return e
}

// doJSON builds an http.Request with a JSON body and serves it
// against the given handler, returning the response recorder.
func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// mustRouter calls h.Router() and fails the test if it returns an error.
func mustRouter(t *testing.T, h *Handler) http.Handler {
	t.Helper()
	r, err := h.Router()
	if err != nil {
		t.Fatalf("Router(): %v", err)
	}
	return r
}

// ── Environment tests ─────────────────────────────────────────────

func TestCreateEnvironment_happyPath(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		Spec:           ResourceSpec{CPUs: 2, RamMB: 1024},
		ManifestInline: encodeManifest(t),
	})

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if env.ID != "fuse-task-1" {
		t.Errorf("id = %q, want fuse-task-1", env.ID)
	}
	if env.State != "running" {
		t.Errorf("state = %q, want running", env.State)
	}
	if env.TaskID != "task-1" {
		t.Errorf("task_id = %q", env.TaskID)
	}
}

func TestCreateEnvironment_duplicateTaskReturns409(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	body := CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	}
	if rr := doJSON(t, r, http.MethodPost, "/v1/environments", body); rr.Code != http.StatusCreated {
		t.Fatalf("first call status = %d", rr.Code)
	}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", body)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409. body: %s", rr.Code, rr.Body.String())
	}
	env := decodeError(t, rr.Body)
	if env.Error.Code != CodeConflict {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeConflict)
	}
}

func TestCreateEnvironment_missingManifestUsesDefault(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID: "task-1",
		// ManifestInline intentionally omitted
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
}

func TestCreateEnvironment_missingTaskIDReturns400(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateEnvironment_badTimeoutsReturn400(t *testing.T) {
	cases := []struct {
		name string
		spec ResourceSpec
	}{
		{"negative max runtime", ResourceSpec{MaxRuntimeSeconds: -3600}},
		{"negative idle timeout", ResourceSpec{IdleTimeoutSeconds: -900}},
		{"sub-minute idle timeout", ResourceSpec{IdleTimeoutSeconds: 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newTestHandler(t)
			r := mustRouter(t, h)

			rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
				TaskID:         "task-1",
				Spec:           tc.spec,
				ManifestInline: encodeManifest(t),
			})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestCreateEnvironment_invalidBase64Returns400(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: "not base64!",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestGetEnvironment_notFoundReturns404(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodGet, "/v1/environments/fuse-missing", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	env := decodeError(t, rr.Body)
	if env.Error.Code != CodeNotFound {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestGetEnvironment_happyPath(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	// Provision first.
	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodGet, "/v1/environments/fuse-task-1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var env Environment
	_ = json.NewDecoder(rr.Body).Decode(&env)
	if env.ID != "fuse-task-1" {
		t.Errorf("id = %q", env.ID)
	}
}

func TestListEnvironments_filtersByTaskID(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodGet, "/v1/environments?task_id=task-1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var out EnvironmentList
	_ = json.NewDecoder(rr.Body).Decode(&out)
	if len(out.Environments) != 1 {
		t.Fatalf("len = %d, want 1", len(out.Environments))
	}
	if out.Environments[0].TaskID != "task-1" {
		t.Errorf("task_id = %q", out.Environments[0].TaskID)
	}
}

func TestListEnvironments_paginatesWithLimitAndCursor(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	for i := 0; i < 5; i++ {
		_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
			TaskID:         fmt.Sprintf("task-%d", i),
			ManifestInline: encodeManifest(t),
		})
	}

	rr := doJSON(t, r, http.MethodGet, "/v1/environments?limit=2", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var page1 EnvironmentList
	_ = json.NewDecoder(rr.Body).Decode(&page1)
	if len(page1.Environments) != 2 {
		t.Fatalf("page 1 len = %d, want 2", len(page1.Environments))
	}
	if page1.NextCursor == nil || *page1.NextCursor == "" {
		t.Fatal("expected a next_cursor on page 1")
	}

	seen := map[string]bool{}
	for _, e := range page1.Environments {
		seen[e.ID] = true
	}

	cursor := *page1.NextCursor
	for {
		rr := doJSON(t, r, http.MethodGet, "/v1/environments?limit=2&cursor="+cursor, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
		}
		var page EnvironmentList
		_ = json.NewDecoder(rr.Body).Decode(&page)
		for _, e := range page.Environments {
			if seen[e.ID] {
				t.Fatalf("id %s returned twice across pages", e.ID)
			}
			seen[e.ID] = true
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}

	if len(seen) != 5 {
		t.Fatalf("saw %d unique environments across pages, want 5", len(seen))
	}
}

func TestListEnvironments_invalidCursorReturns400(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodGet, "/v1/environments?cursor=not-valid-base64!!", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

func TestDestroyEnvironment_returns204AndMissingReturns404(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodDelete, "/v1/environments/fuse-task-1", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204. body: %s", rr.Code, rr.Body.String())
	}

	rr = doJSON(t, r, http.MethodDelete, "/v1/environments/does-not-exist", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestListEnvironments_returnsAllTracked(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	for _, id := range []string{"a", "b", "c"} {
		_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
			TaskID:         id,
			ManifestInline: encodeManifest(t),
		})
	}

	rr := doJSON(t, r, http.MethodGet, "/v1/environments", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var out EnvironmentList
	_ = json.NewDecoder(rr.Body).Decode(&out)
	if len(out.Environments) != 3 {
		t.Errorf("got %d envs, want 3", len(out.Environments))
	}
}

// ── Snapshot tests ────────────────────────────────────────────────

func TestSnapshotLifecycle_createListRestore(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	// Provision VM.
	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	// Create snapshot.
	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1/snapshots", CreateSnapshotRequest{
		Comment: "before-migration",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d. body: %s", rr.Code, rr.Body.String())
	}
	var snap Snapshot
	_ = json.NewDecoder(rr.Body).Decode(&snap)
	if snap.ID != "cp-1" {
		t.Errorf("snapshot id = %q, want cp-1", snap.ID)
	}
	if snap.VMID != "fuse-task-1" {
		t.Errorf("vm_id = %q", snap.VMID)
	}
	if snap.Comment != "before-migration" {
		t.Errorf("comment = %q", snap.Comment)
	}
	if snap.State != "ready" {
		t.Errorf("state = %q, want ready", snap.State)
	}

	// List snapshots.
	rr = doJSON(t, r, http.MethodGet, "/v1/snapshots?vm_id=fuse-task-1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d", rr.Code)
	}
	var list SnapshotList
	_ = json.NewDecoder(rr.Body).Decode(&list)
	if len(list.Snapshots) != 1 {
		t.Fatalf("len = %d, want 1", len(list.Snapshots))
	}
	if list.Snapshots[0].ID != "cp-1" {
		t.Errorf("id = %q", list.Snapshots[0].ID)
	}

	// Get snapshot.
	rr = doJSON(t, r, http.MethodGet, "/v1/snapshots/cp-1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status = %d", rr.Code)
	}

	// Restore snapshot.
	rr = doJSON(t, r, http.MethodPost, "/v1/snapshots/cp-1?action=restore", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("restore status = %d. body: %s", rr.Code, rr.Body.String())
	}
}

// TestSnapshot_kindOnEveryReadPath pins `kind` to every place a snapshot can
// be read, not just the create response. The SDKs tell callers to check kind
// rather than assume their request was honoured, which is only advice they can
// take if get and list carry it too.
func TestSnapshot_kindOnEveryReadPath(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1/snapshots", CreateSnapshotRequest{})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d. body: %s", rr.Code, rr.Body.String())
	}
	var created Snapshot
	_ = json.NewDecoder(rr.Body).Decode(&created)
	if created.Kind != "disk" {
		t.Errorf("create kind = %q, want disk", created.Kind)
	}

	rr = doJSON(t, r, http.MethodGet, "/v1/snapshots/cp-1", nil)
	var got Snapshot
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Kind != "disk" {
		t.Errorf("get kind = %q, want disk", got.Kind)
	}

	rr = doJSON(t, r, http.MethodGet, "/v1/snapshots?vm_id=fuse-task-1", nil)
	var list SnapshotList
	_ = json.NewDecoder(rr.Body).Decode(&list)
	if len(list.Snapshots) != 1 {
		t.Fatalf("len = %d, want 1", len(list.Snapshots))
	}
	if list.Snapshots[0].Kind != "disk" {
		t.Errorf("list kind = %q, want disk", list.Snapshots[0].Kind)
	}
}

// TestSnapshot_kindIsAlwaysSerialized guards the wire shape rather than the
// decoded struct: `kind` has no omitempty precisely so a client can read it
// unconditionally, and an accidental omitempty would look fine to every test
// that decodes into Snapshot.
func TestSnapshot_kindIsAlwaysSerialized(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1/snapshots", CreateSnapshotRequest{})
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw body: %v", err)
	}
	if raw["kind"] != "disk" {
		t.Errorf("raw kind = %v, want \"disk\". body: %s", raw["kind"], rr.Body.String())
	}
}

// TestSnapshot_liveRequestRecordsLiveKind covers the happy path: a provider
// that implements LiveSnapshotCapable takes the live checkpoint and the
// record says so.
func TestSnapshot_liveRequestRecordsLiveKind(t *testing.T) {
	h, _ := newLiveTestHandler(t, false)
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1/snapshots", CreateSnapshotRequest{
		Live:    true,
		Comment: "warmed cache",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var snap Snapshot
	_ = json.NewDecoder(rr.Body).Decode(&snap)
	if snap.Kind != "live" {
		t.Errorf("kind = %q, want live", snap.Kind)
	}

	// and it survives the round trip through the store, not just the create
	// response the handler happens to hold in memory.
	rr = doJSON(t, r, http.MethodGet, "/v1/snapshots/"+snap.ID, nil)
	var got Snapshot
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Kind != "live" {
		t.Errorf("get kind = %q, want live", got.Kind)
	}
}

// TestSnapshot_liveRequestServedAsDiskReportsDisk is the contract the SDK docs
// lean on: a host agent too old to honour live answers with a disk snapshot,
// and the record must say disk. Echoing the request back would file a
// cold-booting artifact as a resumable one.
func TestSnapshot_liveRequestServedAsDiskReportsDisk(t *testing.T) {
	h, _ := newLiveTestHandler(t, true)
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1/snapshots", CreateSnapshotRequest{Live: true})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var snap Snapshot
	_ = json.NewDecoder(rr.Body).Decode(&snap)
	if snap.Kind != "disk" {
		t.Errorf("kind = %q, want disk: the record must report what was written, not what was asked for", snap.Kind)
	}
}

func TestSnapshot_restoreUnknownSnapshotReturns404(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	rr := doJSON(t, r, http.MethodPost, "/v1/snapshots/never-existed?action=restore", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestSnapshot_listForUnknownVMReturnsEmptyList(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodGet, "/v1/snapshots?vm_id=nope", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out SnapshotList
	_ = json.NewDecoder(rr.Body).Decode(&out)
	if len(out.Snapshots) != 0 {
		t.Fatalf("len = %d, want 0", len(out.Snapshots))
	}
}

func TestSnapshot_deleteLifecycle(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})
	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1/snapshots", CreateSnapshotRequest{
		Comment: "delete-me",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rr.Code)
	}

	rr = doJSON(t, r, http.MethodDelete, "/v1/snapshots/cp-1", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d. body: %s", rr.Code, rr.Body.String())
	}

	rr = doJSON(t, r, http.MethodGet, "/v1/snapshots/cp-1", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("get-after-delete status = %d, want 404", rr.Code)
	}
}

func TestSnapshot_quotaExceededReturns409(t *testing.T) {
	p := newFakeProvider()
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider:              p,
		Prefix:                "fuse-",
		SnapshotQuotaMaxCount: 1,
	})
	h := &Handler{Fleet: fm}
	r := mustRouter(t, h)

	_ = doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	})

	if rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1/snapshots", CreateSnapshotRequest{}); rr.Code != http.StatusCreated {
		t.Fatalf("first create status = %d", rr.Code)
	}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1/snapshots", CreateSnapshotRequest{})
	if rr.Code != http.StatusConflict {
		t.Fatalf("quota status = %d, want 409", rr.Code)
	}
}

// ── Resolver tests ────────────────────────────────────────────────

func TestInlineResolver_defaultsEmpty(t *testing.T) {
	got, err := InlineResolver{}.Resolve(CreateEnvironmentRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(defaultManifest) {
		t.Errorf("got %q, want default manifest", string(got))
	}
}

func TestInlineResolver_decodesBase64(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("hello"))
	got, err := InlineResolver{}.Resolve(CreateEnvironmentRequest{ManifestInline: encoded})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}

func TestInlineResolver_rejectsInvalidBase64(t *testing.T) {
	_, err := InlineResolver{}.Resolve(CreateEnvironmentRequest{ManifestInline: "!!!"})
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

// ── Drain action tests ────────────────────────────────────────────
//
// Drain delegates to FleetManager.Drain, which runs the configured
// DrainCommand inside the guest via Environment.ExecStream. Provisioning goes
// through the real ProvisionAndAssign → Boot → FusedAgentSpec path, so the VM
// picks up DefaultFusedDrainCommand automatically. These tests assert the HTTP
// contract and that the command was Exec'd on the env the Fleet actually holds.

// drainExecCount returns how many ExecStream calls the provisioned env
// recorded, asserting the env exists.
func drainExecCount(t *testing.T, p *fakeProvider, name string) int {
	t.Helper()
	p.mu.Lock()
	e, ok := p.envs[name]
	p.mu.Unlock()
	if !ok {
		t.Fatalf("fake env %q not found", name)
	}
	return len(e.execCommands())
}

func TestEnvironmentAction_Drain_HappyPath(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)

	if rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rr.Code, rr.Body.String())
	}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=drain", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("drain status = %d, want 200. body: %s", rr.Code, rr.Body.String())
	}

	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if env.State != "draining" {
		t.Errorf("state = %q, want draining", env.State)
	}
	if env.ID != "fuse-task-1" {
		t.Errorf("id = %q", env.ID)
	}
	if got := drainExecCount(t, p, "fuse-task-1"); got != 1 {
		t.Errorf("drain command exec count = %d, want 1", got)
	}
}

func TestEnvironmentAction_Drain_NotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-missing?action=drain", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body: %s", rr.Code, rr.Body.String())
	}
}

func TestEnvironmentAction_Drain_AlreadyDrainingReturns409(t *testing.T) {
	h, _, p := newTestHandler(t)
	r := mustRouter(t, h)

	if rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rr.Code)
	}

	if rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=drain", nil); rr.Code != http.StatusOK {
		t.Fatalf("first drain status = %d", rr.Code)
	}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=drain", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("second drain status = %d, want 409. body: %s", rr.Code, rr.Body.String())
	}
	// The rejected second drain must not run the command again.
	if got := drainExecCount(t, p, "fuse-task-1"); got != 1 {
		t.Errorf("drain command exec count = %d, want 1 (second drain must not exec)", got)
	}
}

func TestEnvironmentAction_Drain_ThenDeleteSucceeds(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	if rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		ManifestInline: encodeManifest(t),
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create: %d", rr.Code)
	}
	if rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=drain", nil); rr.Code != http.StatusOK {
		t.Fatalf("drain: %d", rr.Code)
	}
	if rr := doJSON(t, r, http.MethodDelete, "/v1/environments/fuse-task-1", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete after drain: %d body=%s", rr.Code, rr.Body.String())
	}
}

// ── Fork action ───────────────────────────────────────────────────

// CreateFromCheckpoint makes fakeProvider satisfy orchestrator.SnapshotForkable
// so the fork happy path is exercisable end to end: it seeds a new env from the
// source env's matching checkpoint.
func (p *fakeProvider) CreateFromCheckpoint(_ context.Context, spec orchestrator.Spec, srcVMID, checkpointID string) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	src, ok := p.envs[srcVMID]
	if !ok {
		return nil, fmt.Errorf("source env not found: %s", srcVMID)
	}
	e := &fakeEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}
	src.mu.Lock()
	for _, cp := range src.checkpoints {
		if cp.ID == checkpointID {
			e.checkpoints = append(e.checkpoints, cp)
		}
	}
	src.mu.Unlock()
	p.envs[spec.Name] = e
	return e, nil
}

func TestEnvironmentAction_Fork_HappyPath(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	if rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-1",
		Spec:           ResourceSpec{CPUs: 2, RamMB: 1024},
		ManifestInline: encodeManifest(t),
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create source: code = %d", rr.Code)
	}

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-task-1?action=fork", ForkEnvironmentRequest{Comment: "clone"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("fork: code = %d, body = %s", rr.Code, rr.Body.String())
	}
	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode environment: %v", err)
	}
	if env.ID == "" || env.ID == "fuse-task-1" {
		t.Errorf("fork id = %q, want a new distinct id", env.ID)
	}
	if env.State != "running" {
		t.Errorf("fork state = %q, want running", env.State)
	}
}

func TestEnvironmentAction_Fork_NotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments/fuse-missing?action=fork", ForkEnvironmentRequest{})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body = %s", rr.Code, rr.Body.String())
	}
}

// ── Host registration ─────────────────────────────────────────────

// newTestHandlerWithProvider is like newTestHandler but also wires
// NewProvider so registerHost (which requires a provider factory) is
// reachable.
func newTestHandlerWithProvider(t *testing.T) (*Handler, *orchestrator.FleetManager) {
	t.Helper()
	p := newFakeProvider()
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})
	h := &Handler{
		Fleet: fm,
		NewProvider: func(url, token string, backend orchestrator.HostBackend) orchestrator.Provider {
			return newFakeProvider()
		},
	}
	return h, fm
}

func TestRegisterHost_GPUAndBackendRoundTrip(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:      "host-1",
		URL:     "http://host-1.test",
		Backend: "qemu",
		Capacity: HostCapacity{
			CPUs:      4,
			RamMB:     8192,
			StorageGB: 100,
			VMCount:   10,
			GPUs:      2,
			GPUKind:   "a100",
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}

	var info HostInfo
	if err := json.NewDecoder(rr.Body).Decode(&info); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if info.Backend != "qemu" {
		t.Errorf("backend = %q, want qemu", info.Backend)
	}
	if info.Capacity.GPUs != 2 {
		t.Errorf("capacity.gpus = %d, want 2", info.Capacity.GPUs)
	}
	if info.Capacity.GPUKind != "a100" {
		t.Errorf("capacity.gpu_kind = %q, want a100", info.Capacity.GPUKind)
	}
}

func TestRegisterHost_AbsentFieldsDefaultToFirecrackerAndZeroGPUs(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:  "host-2",
		URL: "http://host-2.test",
		Capacity: HostCapacity{
			CPUs:      4,
			RamMB:     8192,
			StorageGB: 100,
			VMCount:   10,
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}

	var info HostInfo
	if err := json.NewDecoder(rr.Body).Decode(&info); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if info.Backend != "firecracker" {
		t.Errorf("backend = %q, want firecracker (default)", info.Backend)
	}
	if info.Capacity.GPUs != 0 || info.Capacity.GPUKind != "" {
		t.Errorf("capacity gpu fields = %+v, want zero values", info.Capacity)
	}
}

func TestRegisterHost_GPUsWithoutQEMUBackendReturns400(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:      "host-3",
		URL:     "http://host-3.test",
		Backend: "firecracker",
		Capacity: HostCapacity{
			CPUs:      4,
			RamMB:     8192,
			StorageGB: 100,
			VMCount:   10,
			GPUs:      1,
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
	env := decodeError(t, rr.Body)
	if env.Error.Code != CodeInvalidArgument {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeInvalidArgument)
	}
}

func TestRegisterHost_GPUsWithDefaultBackendReturns400(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	// Backend omitted (defaults to firecracker) but gpus > 0 requested.
	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:  "host-4",
		URL: "http://host-4.test",
		Capacity: HostCapacity{
			CPUs:      4,
			RamMB:     8192,
			StorageGB: 100,
			VMCount:   10,
			GPUs:      1,
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

func TestRegisterHost_ProviderFactoryReceivesBackend(t *testing.T) {
	p := newFakeProvider()
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})
	var gotBackend orchestrator.HostBackend
	h := &Handler{
		Fleet: fm,
		NewProvider: func(url, token string, backend orchestrator.HostBackend) orchestrator.Provider {
			gotBackend = backend
			return newFakeProvider()
		},
	}
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:      "host-qemu",
		URL:     "http://host-qemu.test",
		Backend: "qemu",
		Capacity: HostCapacity{
			CPUs:      4,
			RamMB:     8192,
			StorageGB: 100,
			VMCount:   10,
			GPUs:      1,
			GPUKind:   "a100",
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	if gotBackend != orchestrator.BackendQEMU {
		t.Errorf("factory backend = %q, want qemu", gotBackend)
	}
}

func TestRegisterHost_RejectsUnknownBackend(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:      "host-invalid",
		URL:     "http://host-invalid.test",
		Backend: "unknown",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

func TestRegisterHost_MissingVMCountReturns400(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:  "host-no-vmcount",
		URL: "http://host-no-vmcount.test",
		Capacity: HostCapacity{
			CPUs:      4,
			RamMB:     8192,
			StorageGB: 100,
			// VMCount omitted: never probed, always required.
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
	env := decodeError(t, rr.Body)
	if env.Error.Code != CodeInvalidArgument {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeInvalidArgument)
	}
}

func TestRegisterHost_NegativeGPUsReturns400(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:      "host-negative-gpus",
		URL:     "http://host-negative-gpus.test",
		Backend: "qemu",
		Capacity: HostCapacity{
			CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 10, GPUs: -1,
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

// TestRegisterHost_NoProviderNoCapacityReturns400 is the pre-existing
// behavior when the provider cannot probe (fakeProvider does not implement
// CapacityProber) and the operator declared no capacity at all: registration
// must fail rather than silently accepting a zero-capacity host.
func TestRegisterHost_NoProviderNoCapacityReturns400(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:  "host-no-capacity",
		URL: "http://host-no-capacity.test",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
	env := decodeError(t, rr.Body)
	if env.Error.Code != CodeInvalidArgument {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeInvalidArgument)
	}
}

// ── Host registration: capacity probing (issue #22) ────────────────

// newTestHandlerWithCapacityProvider is like newTestHandlerWithProvider but
// NewProvider returns a capacityProbeProvider so registerHost's probe path
// is reachable.
func newTestHandlerWithCapacityProvider(t *testing.T, capacity orchestrator.HostCapacity, probeErr error) (*Handler, *orchestrator.FleetManager) {
	t.Helper()
	p := newFakeProvider()
	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider: p,
		Prefix:   "fuse-",
	})
	h := &Handler{
		Fleet: fm,
		NewProvider: func(url, token string, backend orchestrator.HostBackend) orchestrator.Provider {
			return &capacityProbeProvider{fakeProvider: newFakeProvider(), capacity: capacity, err: probeErr}
		},
	}
	return h, fm
}

// TestRegisterHost_ProbesCapacityWhenUnset covers proposal item 1+2: omitted
// cpus/ram_mb/storage_gb are sourced from the host agent's probe, not left
// at zero.
func TestRegisterHost_ProbesCapacityWhenUnset(t *testing.T) {
	h, _ := newTestHandlerWithCapacityProvider(t, orchestrator.HostCapacity{CPUs: 8, RamMB: 16384, StorageGB: 200}, nil)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:  "host-probed",
		URL: "http://host-probed.test",
		Capacity: HostCapacity{
			VMCount: 10, // must still be declared explicitly
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var info HostInfo
	if err := json.NewDecoder(rr.Body).Decode(&info); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if info.Capacity.CPUs != 8 || info.Capacity.RamMB != 16384 || info.Capacity.StorageGB != 200 {
		t.Errorf("capacity = %+v, want probed 8/16384/200", info.Capacity)
	}
	if len(info.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", info.Warnings)
	}
}

// TestRegisterHost_DeclaredOverridesProbeWithWarning covers proposal item 2+3:
// an explicit override wins over the probe but is flagged when it exceeds
// what the host actually has.
func TestRegisterHost_DeclaredOverridesProbeWithWarning(t *testing.T) {
	h, _ := newTestHandlerWithCapacityProvider(t, orchestrator.HostCapacity{CPUs: 8, RamMB: 16384, StorageGB: 200}, nil)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:  "host-override",
		URL: "http://host-override.test",
		Capacity: HostCapacity{
			CPUs:    32, // declared, exceeds probed 8
			VMCount: 10,
			// ram_mb/storage_gb omitted -> probed
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var info HostInfo
	if err := json.NewDecoder(rr.Body).Decode(&info); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if info.Capacity.CPUs != 32 {
		t.Errorf("capacity.cpus = %d, want 32 (declared override kept)", info.Capacity.CPUs)
	}
	if info.Capacity.RamMB != 16384 || info.Capacity.StorageGB != 200 {
		t.Errorf("capacity ram/storage = %d/%d, want probed 16384/200", info.Capacity.RamMB, info.Capacity.StorageGB)
	}
	if len(info.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", info.Warnings)
	}
}

// TestRegisterHost_ProbeFailsWithoutFullDeclaredCapacityReturns502 covers the
// case where the agent can't be probed and the operator didn't fully declare
// capacity as a fallback: registration must fail loudly rather than admit a
// host with guessed numbers.
func TestRegisterHost_ProbeFailsWithoutFullDeclaredCapacityReturns502(t *testing.T) {
	h, _ := newTestHandlerWithCapacityProvider(t, orchestrator.HostCapacity{}, fmt.Errorf("connection refused"))
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:  "host-probe-down",
		URL: "http://host-probe-down.test",
		Capacity: HostCapacity{
			VMCount: 10,
			// cpus/ram_mb/storage_gb all unset and unprobeable.
		},
	})
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502. body: %s", rr.Code, rr.Body.String())
	}
}

// TestRegisterHost_ProbeFailsButDeclaredCapacityCompleteSucceeds asserts a
// down/unreachable host agent doesn't block registration when the operator
// already declared full capacity explicitly.
func TestRegisterHost_ProbeFailsButDeclaredCapacityCompleteSucceeds(t *testing.T) {
	h, _ := newTestHandlerWithCapacityProvider(t, orchestrator.HostCapacity{}, fmt.Errorf("connection refused"))
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:  "host-probe-down-declared",
		URL: "http://host-probe-down-declared.test",
		Capacity: HostCapacity{
			CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 10,
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
}

// TestRegisterHost_AgentRejectsTokenReturns401 asserts that a host agent
// rejecting the orchestrator's token (a 401 from the capacity/auth probe)
// hard-refuses the registration, even when the operator declared full
// capacity explicitly -- an unauthenticated host is useless, and this turns
// a later opaque create-time 401 into a clear, layer-named error.
func TestRegisterHost_AgentRejectsTokenReturns401(t *testing.T) {
	h, _ := newTestHandlerWithCapacityProvider(t, orchestrator.HostCapacity{},
		&orchestrator.HTTPStatusError{Code: http.StatusUnauthorized, Body: "unauthorized"})
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:  "host-bad-token",
		URL: "http://host-bad-token.test",
		Capacity: HostCapacity{
			// Fully declared: proves the refusal is driven by the 401, not
			// by missing capacity.
			CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 10,
		},
	})
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502. body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "rejected the token") {
		t.Fatalf("body should name the token layer, got: %s", rr.Body.String())
	}
}

// ── Environment create: GPU fields ─────────────────────────────────

func TestCreateEnvironment_GPUFieldsRoundTrip(t *testing.T) {
	h, fm, provider := newTestHandler(t)
	if err := fm.RegisterHost(context.Background(), orchestrator.Host{
		ID:      "gpu-host",
		Backend: orchestrator.BackendQEMU,
		Capacity: orchestrator.HostCapacity{
			CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 10, GPUs: 1, GPUKind: "a100",
		},
	}, provider); err != nil {
		t.Fatal(err)
	}
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-gpu",
		Spec:           ResourceSpec{CPUs: 2, RamMB: 1024, GPUs: 1, GPUKind: "a100"},
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if env.Spec.GPUs != 1 {
		t.Errorf("spec.gpus = %d, want 1", env.Spec.GPUs)
	}
	if env.Spec.GPUKind != "a100" {
		t.Errorf("spec.gpu_kind = %q, want a100", env.Spec.GPUKind)
	}
}

func TestCreateEnvironment_GPUWithoutHostReturnsUnavailable(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-gpu",
		Spec:           ResourceSpec{GPUs: 1},
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503. body: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateEnvironment_GPUFieldsAbsentDefaultToZero(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-no-gpu",
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if env.Spec.GPUs != 0 || env.Spec.GPUKind != "" {
		t.Errorf("spec gpu fields = %+v, want zero values", env.Spec)
	}
}

func TestCreateEnvironment_InvalidGPUProfileReturns400(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-bad-profile",
		Spec:           ResourceSpec{GPUs: 1, GPUProfile: "half"},
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateEnvironment_GPUProfileWithoutCountReturns400(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-profile-no-count",
		Spec:           ResourceSpec{GPUProfile: "1g.10gb"},
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateEnvironment_GPUProfileOnNonMIGKindReturns400(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-profile-bad-kind",
		Spec:           ResourceSpec{GPUs: 1, GPUKind: "v100", GPUProfile: "1g.10gb"},
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateEnvironment_GPUProfileRoundTrip(t *testing.T) {
	h, fm, provider := newTestHandler(t)
	if err := fm.RegisterHost(context.Background(), orchestrator.Host{
		ID:      "mig-host",
		Backend: orchestrator.BackendQEMU,
		Capacity: orchestrator.HostCapacity{
			CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 10,
			GPUKind:     "a100",
			MIGProfiles: map[string]int{"1g.10gb": 4},
		},
	}, provider); err != nil {
		t.Fatal(err)
	}
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID: "task-mig",
		Spec: ResourceSpec{
			CPUs: 2, RamMB: 1024, GPUs: 1, GPUKind: "a100", GPUProfile: "1G.10GB",
		},
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if env.Spec.GPUProfile != "1g.10gb" {
		t.Errorf("spec.gpu_profile = %q, want 1g.10gb (lowercased)", env.Spec.GPUProfile)
	}
}

func TestRegisterHost_MIGProfilesRequireQEMU(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID: "fc-mig", URL: "http://host", Backend: "firecracker",
		Capacity: HostCapacity{
			CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 10,
			MIGProfiles: map[string]int{"1g.10gb": 2},
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

func TestRegisterHost_MIGProfilesRoundTrip(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID: "mig-host", URL: "http://host", Backend: "qemu",
		Capacity: HostCapacity{
			CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 10, GPUKind: "a100",
			MIGProfiles: map[string]int{"1G.10GB": 4},
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var info HostInfo
	if err := json.NewDecoder(rr.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Capacity.MIGProfiles["1g.10gb"] != 4 {
		t.Errorf("mig_profiles = %v, want {1g.10gb: 4}", info.Capacity.MIGProfiles)
	}
}

// ── Placement tests (issue #102) ──────────────────────────────────

func TestCreateEnvironment_hostPinRoundTripsAndPlaces(t *testing.T) {
	h, fm, provider := newTestHandler(t)
	for _, id := range []string{"build-2", "build-3"} {
		if err := fm.RegisterHost(context.Background(), orchestrator.Host{
			ID:       id,
			Labels:   map[string]string{"disk": "nvme"},
			Capacity: orchestrator.HostCapacity{CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 10},
		}, provider); err != nil {
			t.Fatal(err)
		}
	}
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID: "task-pin",
		Spec: ResourceSpec{
			CPUs: 2, RamMB: 1024,
			HostID: "build-3",
			Labels: map[string]string{"disk": "nvme"},
		},
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var env Environment
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if env.HostID != "build-3" {
		t.Errorf("host_id = %q, want build-3 (the pin)", env.HostID)
	}
	if env.Spec.HostID != "build-3" || env.Spec.Labels["disk"] != "nvme" {
		t.Errorf("spec placement = %+v, want it echoed back", env.Spec)
	}
}

func TestCreateEnvironment_unknownHostPinReturns404(t *testing.T) {
	h, fm, provider := newTestHandler(t)
	if err := fm.RegisterHost(context.Background(), orchestrator.Host{
		ID:       "build-2",
		Capacity: orchestrator.HostCapacity{CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 10},
	}, provider); err != nil {
		t.Fatal(err)
	}
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-bad-pin",
		Spec:           ResourceSpec{CPUs: 2, RamMB: 1024, HostID: "nope"},
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body: %s", rr.Code, rr.Body.String())
	}
	if got := decodeError(t, rr.Body); got.Error.Code != CodeNotFound {
		t.Errorf("code = %q, want %q", got.Error.Code, CodeNotFound)
	}
	// no VM row may exist for a host that cannot exist.
	if vms := fm.ListFleet(); len(vms) != 0 {
		t.Errorf("fleet = %+v, want no vm created", vms)
	}
}

func TestCreateEnvironment_pinToCordonedHostReturnsUnavailable(t *testing.T) {
	h, fm, provider := newTestHandler(t)
	if err := fm.RegisterHost(context.Background(), orchestrator.Host{
		ID:       "build-3",
		State:    orchestrator.HostCordoned,
		Capacity: orchestrator.HostCapacity{CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 10},
	}, provider); err != nil {
		t.Fatal(err)
	}
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-cordoned-pin",
		Spec:           ResourceSpec{CPUs: 2, RamMB: 1024, HostID: "build-3"},
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503. body: %s", rr.Code, rr.Body.String())
	}
	// the message must name the host and the gate, not just "no capacity".
	if body := rr.Body.String(); !strings.Contains(body, "build-3") || !strings.Contains(body, "cordoned") {
		t.Errorf("body = %s, want it to name the host and the cordoned gate", body)
	}
}

func TestCreateEnvironment_labelMissReturnsUnavailable(t *testing.T) {
	h, fm, provider := newTestHandler(t)
	if err := fm.RegisterHost(context.Background(), orchestrator.Host{
		ID:       "build-2",
		Labels:   map[string]string{"disk": "ssd"},
		Capacity: orchestrator.HostCapacity{CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 10},
	}, provider); err != nil {
		t.Fatal(err)
	}
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID: "task-label-miss",
		Spec: ResourceSpec{
			CPUs: 2, RamMB: 1024,
			Labels: map[string]string{"disk": "nvme"},
		},
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503. body: %s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "disk=nvme") {
		t.Errorf("body = %s, want it to name the unmatched selector", body)
	}
}

func TestCreateEnvironment_negativeStartupTimeoutReturns400(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	// A negative bound would reach the fleet as "unset" and silently restore
	// the default, so it must be refused where the sign is still visible.
	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:                      "task-1",
		ManifestInline:              encodeManifest(t),
		StartupScriptTimeoutSeconds: -1,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

// A bound over the fleet ceiling surfaces as a 400 rather than a 500: it is a
// caller mistake with an actionable message, not a server fault.
func TestCreateEnvironment_startupTimeoutOverCeilingReturns400(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:                      "task-1",
		ManifestInline:              encodeManifest(t),
		StartupScriptTimeoutSeconds: 3600,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateEnvironment_invalidPlacementLabelReturns400(t *testing.T) {
	h, _, _ := newTestHandler(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/environments", CreateEnvironmentRequest{
		TaskID:         "task-bad-label",
		Spec:           ResourceSpec{CPUs: 2, RamMB: 1024, Labels: map[string]string{"bad key": "nvme"}},
		ManifestInline: encodeManifest(t),
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

func TestRegisterHost_labelsRoundTrip(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:       "build-3",
		URL:      "http://build-3.test",
		Token:    "agent-token",
		Labels:   map[string]string{"disk": "nvme", "tier": "build"},
		Capacity: HostCapacity{CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 10},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var info HostInfo
	if err := json.NewDecoder(rr.Body).Decode(&info); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if info.Labels["disk"] != "nvme" || info.Labels["tier"] != "build" {
		t.Errorf("labels = %v, want disk=nvme and tier=build", info.Labels)
	}
}

func TestRegisterHost_invalidLabelReturns400(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:       "build-3",
		URL:      "http://build-3.test",
		Token:    "agent-token",
		Labels:   map[string]string{"disk": "bad value"},
		Capacity: HostCapacity{CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 10},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
}

func TestRegisterHost_InvalidArchReturns400(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:  "host-bad-arch",
		URL: "http://host-bad-arch.test",
		Capacity: HostCapacity{
			CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 5,
			Arch: "sparc",
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
	env := decodeError(t, rr.Body)
	if env.Error.Code != CodeInvalidArgument {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeInvalidArgument)
	}
}

func TestRegisterHost_ArchNormalizedAndRoundTripped(t *testing.T) {
	h, _ := newTestHandlerWithProvider(t)
	r := mustRouter(t, h)

	// uname -m spelling in, goarch vocabulary out.
	rr := doJSON(t, r, http.MethodPost, "/v1/hosts", RegisterHostRequest{
		ID:  "host-arm",
		URL: "http://host-arm.test",
		Capacity: HostCapacity{
			CPUs: 4, RamMB: 8192, StorageGB: 100, VMCount: 5,
			Arch: "aarch64",
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
	var info HostInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Capacity.Arch != "arm64" {
		t.Errorf("arch = %q, want %q", info.Capacity.Arch, "arm64")
	}
}
