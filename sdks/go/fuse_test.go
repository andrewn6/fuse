package fuse

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// recordedRequest captures what the test server saw for one request.
type recordedRequest struct {
	method string
	path   string
	query  string
}

// newTestClient spins an httptest.Server with the given handler and
// returns a Client pointed at it plus a cleanup func.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c, err := New(srv.URL, "tok")
	if err != nil {
		srv.Close()
		t.Fatalf("New: %v", err)
	}
	return c, srv.Close
}

func TestEnvironmentsCreate(t *testing.T) {
	var got recordedRequest
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm-1","state":"running","task_id":"task-1","url":"https://x"}`)
	})
	defer cleanup()

	env, err := c.Environments.Create(context.Background(), CreateRequest{TaskID: "task-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", got.method)
	}
	if got.path != "/v1/environments" {
		t.Fatalf("path = %s, want /v1/environments", got.path)
	}
	if env.ID != "vm-1" || env.State != "running" {
		t.Fatalf("decoded env = %+v", env)
	}
}

func TestEnvironmentsList(t *testing.T) {
	var got recordedRequest
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"environments":[{"id":"vm-1","state":"running","task_id":"task-1","url":"u"},{"id":"vm-2","state":"draining","task_id":"task-1","url":"u"}]}`)
	})
	defer cleanup()

	envs, err := c.Environments.List(context.Background(), ListEnvironmentsOptions{TaskID: "task-1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got.method != http.MethodGet {
		t.Fatalf("method = %s, want GET", got.method)
	}
	if got.path != "/v1/environments" {
		t.Fatalf("path = %s, want /v1/environments", got.path)
	}
	// List auto-paginates (walks every page), so it always sends the max
	// page size alongside the caller's filters.
	if got.query != "limit=200&task_id=task-1" {
		t.Fatalf("query = %s, want limit=200&task_id=task-1", got.query)
	}
	if len(envs) != 2 || envs[0].ID != "vm-1" || envs[1].ID != "vm-2" {
		t.Fatalf("decoded envs = %+v", envs)
	}
}

func TestEnvironmentsDrain(t *testing.T) {
	var got recordedRequest
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"vm-1","state":"draining","task_id":"task-1","url":"u"}`)
	})
	defer cleanup()

	env, err := c.Environments.Drain(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", got.method)
	}
	if got.path != "/v1/environments/vm-1" {
		t.Fatalf("path = %s, want /v1/environments/vm-1", got.path)
	}
	if got.query != "action=drain" {
		t.Fatalf("query = %s, want action=drain", got.query)
	}
	if env.State != "draining" {
		t.Fatalf("decoded env = %+v", env)
	}
}

func TestEnvironmentsRotateToken(t *testing.T) {
	var got recordedRequest
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery}
		w.WriteHeader(http.StatusNoContent)
	})
	defer cleanup()

	if err := c.Environments.RotateToken(context.Background(), "vm-1"); err != nil {
		t.Fatalf("RotateToken: %v", err)
	}
	if got.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", got.method)
	}
	if got.path != "/v1/environments/vm-1" {
		t.Fatalf("path = %s, want /v1/environments/vm-1", got.path)
	}
	if got.query != "action=rotate-token" {
		t.Fatalf("query = %s, want action=rotate-token", got.query)
	}
}

func TestSnapshotsCreate(t *testing.T) {
	var got recordedRequest
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"snap-1","vm_id":"vm-1","created_at":"2024-01-01T00:00:00Z"}`)
	})
	defer cleanup()

	snap, err := c.Snapshots.Create(context.Background(), "vm-1", SnapshotRequest{Comment: "c"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", got.method)
	}
	if got.path != "/v1/environments/vm-1/snapshots" {
		t.Fatalf("path = %s, want /v1/environments/vm-1/snapshots", got.path)
	}
	if snap.ID != "snap-1" || snap.VMID != "vm-1" {
		t.Fatalf("decoded snap = %+v", snap)
	}
}

func TestSnapshotsCreate_sendsLiveAndDecodesKind(t *testing.T) {
	var body []byte
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"snap-1","vm_id":"vm-1","kind":"live","created_at":"2024-01-01T00:00:00Z"}`)
	})
	defer cleanup()

	snap, err := c.Snapshots.Create(context.Background(), "vm-1", SnapshotRequest{Live: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.Contains(string(body), `"live":true`) {
		t.Fatalf("request body missing live flag: %s", body)
	}
	if snap.Kind != "live" {
		t.Fatalf("Kind = %q, want live", snap.Kind)
	}
}

// the flag is a request, not a guarantee: an agent too old to honour it
// answers with a disk snapshot, and the sdk must surface that rather than let
// a caller read its own request back.
func TestSnapshotsCreate_liveServedAsDiskDecodesDiskKind(t *testing.T) {
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"snap-1","vm_id":"vm-1","kind":"disk","created_at":"2024-01-01T00:00:00Z"}`)
	})
	defer cleanup()

	snap, err := c.Snapshots.Create(context.Background(), "vm-1", SnapshotRequest{Live: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if snap.Kind != "disk" {
		t.Fatalf("Kind = %q, want disk", snap.Kind)
	}
}

// an omitted Live must not travel: `"live":false` on every ordinary create
// would be noise, and the server default is already disk.
func TestSnapshotsCreate_omitsLiveWhenUnset(t *testing.T) {
	var body []byte
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"snap-1","vm_id":"vm-1","kind":"disk","created_at":"2024-01-01T00:00:00Z"}`)
	})
	defer cleanup()

	if _, err := c.Snapshots.Create(context.Background(), "vm-1", SnapshotRequest{Comment: "c"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if strings.Contains(string(body), `"live"`) {
		t.Fatalf("live sent when unset: %s", body)
	}
}

func TestSnapshotsList(t *testing.T) {
	var got recordedRequest
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"snapshots":[{"id":"snap-1","vm_id":"vm-1","created_at":"2024-01-01T00:00:00Z"}]}`)
	})
	defer cleanup()

	snaps, err := c.Snapshots.List(context.Background(), ListSnapshotsOptions{VMID: "vm-1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got.method != http.MethodGet {
		t.Fatalf("method = %s, want GET", got.method)
	}
	if len(snaps) != 1 || snaps[0].ID != "snap-1" {
		t.Fatalf("decoded snaps = %+v", snaps)
	}
}

func TestSnapshotsListPage_sendsLayerFilters(t *testing.T) {
	var got recordedRequest
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"snapshots":[]}`)
	})
	defer cleanup()

	// arch travels as its own filter rather than being folded into the layer
	// key, so a lookup that omits it can match an artifact it cannot boot.
	_, err := c.Snapshots.ListPage(context.Background(), ListSnapshotsOptions{
		LayerKey: "abc123",
		Arch:     "arm64",
	})
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	values, err := url.ParseQuery(got.query)
	if err != nil {
		t.Fatalf("parse query %q: %v", got.query, err)
	}
	if values.Get("layer_key") != "abc123" {
		t.Fatalf("layer_key = %q, want abc123", values.Get("layer_key"))
	}
	if values.Get("arch") != "arm64" {
		t.Fatalf("arch = %q, want arm64", values.Get("arch"))
	}
}

func TestSnapshotsListPage_omitsUnsetLayerFilters(t *testing.T) {
	var got recordedRequest
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"snapshots":[]}`)
	})
	defer cleanup()

	// an unset filter must not be sent at all: an empty layer_key would ask
	// the server to match every snapshot that is not a layer.
	if _, err := c.Snapshots.ListPage(context.Background(), ListSnapshotsOptions{VMID: "vm-1"}); err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	values, err := url.ParseQuery(got.query)
	if err != nil {
		t.Fatalf("parse query %q: %v", got.query, err)
	}
	if _, ok := values["layer_key"]; ok {
		t.Fatalf("layer_key sent when unset: %q", got.query)
	}
	if _, ok := values["arch"]; ok {
		t.Fatalf("arch sent when unset: %q", got.query)
	}
}

func TestSnapshotsGet_decodesArtifactFields(t *testing.T) {
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"snap-1","vm_id":"vm-1","mode":"build","layer_key":"abc123","arch":"amd64","digest":"deadbeef","kind":"live","created_at":"2024-01-01T00:00:00Z"}`)
	})
	defer cleanup()

	snap, err := c.Snapshots.Get(context.Background(), "snap-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if snap.LayerKey != "abc123" {
		t.Fatalf("LayerKey = %q, want abc123", snap.LayerKey)
	}
	if snap.Arch != "amd64" {
		t.Fatalf("Arch = %q, want amd64", snap.Arch)
	}
	if snap.Digest != "deadbeef" {
		t.Fatalf("Digest = %q, want deadbeef", snap.Digest)
	}
	// kind has to decode on get, not only on create: the docs tell callers to
	// check it, which they can only do wherever they read the snapshot.
	if snap.Kind != "live" {
		t.Fatalf("Kind = %q, want live", snap.Kind)
	}
}

func TestEnvironmentsListPage_roundTripsCursor(t *testing.T) {
	calls := 0
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			io.WriteString(w, `{"environments":[{"id":"vm-1","state":"running","task_id":"t1","url":"u"}],"next_cursor":"YWJj"}`)
			return
		}
		io.WriteString(w, `{"environments":[{"id":"vm-2","state":"running","task_id":"t2","url":"u"}]}`)
	})
	defer cleanup()

	page, err := c.Environments.ListPage(context.Background(), ListEnvironmentsOptions{Limit: 1})
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	if page.NextCursor != "YWJj" {
		t.Fatalf("next cursor = %q, want YWJj", page.NextCursor)
	}
	if len(page.Environments) != 1 || page.Environments[0].ID != "vm-1" {
		t.Fatalf("page 1 = %+v", page.Environments)
	}

	page2, err := c.Environments.ListPage(context.Background(), ListEnvironmentsOptions{Cursor: page.NextCursor})
	if err != nil {
		t.Fatalf("ListPage page 2: %v", err)
	}
	if page2.NextCursor != "" {
		t.Fatalf("expected no next cursor on last page, got %q", page2.NextCursor)
	}
	if len(page2.Environments) != 1 || page2.Environments[0].ID != "vm-2" {
		t.Fatalf("page 2 = %+v", page2.Environments)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestEnvironmentsList_autoPaginatesAllPages(t *testing.T) {
	calls := 0
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			io.WriteString(w, `{"environments":[{"id":"vm-1","state":"running","url":"u"}],"next_cursor":"YWJj"}`)
			return
		}
		io.WriteString(w, `{"environments":[{"id":"vm-2","state":"running","url":"u"}]}`)
	})
	defer cleanup()

	envs, err := c.Environments.List(context.Background(), ListEnvironmentsOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(envs) != 2 || envs[0].ID != "vm-1" || envs[1].ID != "vm-2" {
		t.Fatalf("decoded envs = %+v", envs)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestSnapshotsRestore(t *testing.T) {
	var got recordedRequest
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery}
		w.WriteHeader(http.StatusNoContent)
	})
	defer cleanup()

	if err := c.Snapshots.Restore(context.Background(), "snap-1"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", got.method)
	}
	if got.path != "/v1/snapshots/snap-1" {
		t.Fatalf("path = %s, want /v1/snapshots/snap-1", got.path)
	}
	if got.query != "action=restore" {
		t.Fatalf("query = %s, want action=restore", got.query)
	}
}

func TestHostsRegister(t *testing.T) {
	var got recordedRequest
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"host-1","url":"https://h","state":"active","capacity":{"cpus":4,"ram_mb":8192,"storage_gb":100,"vm_count":10},"allocated":{"cpus":0,"ram_mb":0,"storage_gb":0,"vm_count":0},"last_seen":"2024-01-01T00:00:00Z","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T00:00:00Z"}`)
	})
	defer cleanup()

	host, err := c.Hosts.Register(context.Background(), RegisterHostRequest{ID: "host-1", URL: "https://h"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", got.method)
	}
	if got.path != "/v1/hosts" {
		t.Fatalf("path = %s, want /v1/hosts", got.path)
	}
	if host.ID != "host-1" {
		t.Fatalf("decoded host = %+v", host)
	}
}

func TestHostsList(t *testing.T) {
	var got recordedRequest
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"hosts":[{"id":"host-1","url":"https://h","state":"active"}]}`)
	})
	defer cleanup()

	hosts, err := c.Hosts.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got.method != http.MethodGet {
		t.Fatalf("method = %s, want GET", got.method)
	}
	if got.path != "/v1/hosts" {
		t.Fatalf("path = %s, want /v1/hosts", got.path)
	}
	if len(hosts) != 1 || hosts[0].ID != "host-1" {
		t.Fatalf("decoded hosts = %+v", hosts)
	}
}

func TestHostsCordon(t *testing.T) {
	var got recordedRequest
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery}
		w.WriteHeader(http.StatusNoContent)
	})
	defer cleanup()

	if err := c.Hosts.Cordon(context.Background(), "host-1"); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	if got.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", got.method)
	}
	if got.path != "/v1/hosts/host-1" {
		t.Fatalf("path = %s, want /v1/hosts/host-1", got.path)
	}
	if got.query != "action=cordon" {
		t.Fatalf("query = %s, want action=cordon", got.query)
	}
}

func TestAPIKeysCreate(t *testing.T) {
	var got recordedRequest
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"key-1","label":"ci","created_at":"2024-01-01T00:00:00Z","key":"secret-raw"}`)
	})
	defer cleanup()

	key, err := c.APIKeys.Create(context.Background(), "ci")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", got.method)
	}
	if got.path != "/v1/api-keys" {
		t.Fatalf("path = %s, want /v1/api-keys", got.path)
	}
	if key.Key != "secret-raw" || key.ID != "key-1" {
		t.Fatalf("decoded key = %+v", key)
	}
}

func TestAPIKeysList(t *testing.T) {
	var got recordedRequest
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"api_keys":[{"id":"key-1","label":"ci","created_at":"2024-01-01T00:00:00Z"}]}`)
	})
	defer cleanup()

	keys, err := c.APIKeys.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got.method != http.MethodGet {
		t.Fatalf("method = %s, want GET", got.method)
	}
	if got.path != "/v1/api-keys" {
		t.Fatalf("path = %s, want /v1/api-keys", got.path)
	}
	if len(keys) != 1 || keys[0].ID != "key-1" {
		t.Fatalf("decoded keys = %+v", keys)
	}
}

func TestCheckResponseNotFound(t *testing.T) {
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"not_found","message":"x"}}`)
	})
	defer cleanup()

	_, err := c.Environments.Get(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("AsAPIError ok = false for err %v", err)
	}
	if apiErr.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", apiErr.Status)
	}
	if apiErr.Code != "not_found" || apiErr.Message != "x" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
	if !IsNotFound(err) {
		t.Fatal("IsNotFound = false, want true")
	}
}

func TestEventsStream(t *testing.T) {
	c, cleanup := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("response writer is not a flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "id: 1\ndata: {\"event\":\"state\",\"vm_id\":\"v\",\"state\":\"running\",\"updated_at\":\"2024-01-01T00:00:00Z\"}\n\n")
		fl.Flush()
		io.WriteString(w, "data: {\"event\":\"state\",\"vm_id\":\"v\",\"state\":\"destroyed\",\"updated_at\":\"2024-01-01T00:00:01Z\"}\n\n")
		fl.Flush()
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := c.Environments.Events(ctx, "v")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	first, ok := <-ch
	if !ok {
		t.Fatal("channel closed before first event")
	}
	if first.Err != nil {
		t.Fatalf("first event err: %v", first.Err)
	}
	if first.State != StateRunning {
		t.Fatalf("first state = %q, want running", first.State)
	}

	second, ok := <-ch
	if !ok {
		t.Fatal("channel closed before second event")
	}
	if second.Err != nil {
		t.Fatalf("second event err: %v", second.Err)
	}
	if second.State != StateDestroyed {
		t.Fatalf("second state = %q, want destroyed", second.State)
	}

	// terminal state must close the channel.
	if third, ok := <-ch; ok {
		t.Fatalf("expected channel close after terminal state, got %+v", third)
	}
}

func TestStatePredicates(t *testing.T) {
	cases := []struct {
		state    string
		terminal bool
		settled  bool
	}{
		{StateProvisioning, false, false},
		{StateRunning, false, true},
		{StateDraining, false, false},
		{StateDestroying, false, false},
		{StateDestroyed, true, true},
		{StateFailed, true, true},
	}
	for _, c := range cases {
		if got := IsTerminalState(c.state); got != c.terminal {
			t.Errorf("IsTerminalState(%q) = %v, want %v", c.state, got, c.terminal)
		}
		if got := IsSettledState(c.state); got != c.settled {
			t.Errorf("IsSettledState(%q) = %v, want %v", c.state, got, c.settled)
		}
	}
}
