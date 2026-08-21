package fuse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

type SnapshotsService struct {
	t *transport
}

func newSnapshotsService(t *transport) *SnapshotsService {
	return &SnapshotsService{t: t}
}

type ListSnapshotsOptions struct {
	VMID     string
	TaskID   string
	TenantID string
	State    string

	// Mode narrows to one creation mode, e.g. "build" for build artifacts.
	Mode string

	// Name matches the snapshot's metadata name, the lookup key `fuse build`
	// sets on an artifact.
	Name string

	// LayerKey narrows to the build layers taken after one setup step. Empty
	// means "do not filter", never "match artifacts with no layer key".
	LayerKey string

	// Arch narrows to artifacts built on one architecture, in GOARCH
	// vocabulary ("amd64", "arm64"). It is a separate filter from LayerKey
	// rather than part of it because a rootfs is not portable across
	// architectures, so a layer lookup that does not constrain arch can be
	// served bytes it cannot boot.
	Arch string

	// Limit caps the page size (server default 50, max 200; <=0 uses the
	// server default). Only consulted by ListPage — List always requests
	// the server's max page size internally since it walks every page.
	Limit int

	// Cursor resumes after a previous page's SnapshotPage.NextCursor. It is
	// an opaque token; treat it as such rather than parsing it.
	Cursor string
}

// SnapshotPage is one page of a List call, plus the cursor to fetch the
// next one.
type SnapshotPage struct {
	Snapshots []Snapshot `json:"snapshots"`
	// NextCursor is empty once there are no more results.
	NextCursor string `json:"next_cursor,omitempty"`
}

// Create checkpoints a running environment. The zero SnapshotRequest takes a
// disk snapshot, the rootfs and nothing else; set Live to capture the guest's
// memory and vCPU state as well.
//
// The returned Snapshot.Kind reports what was actually written, which can be
// "disk" even for a Live request that reached a host too old to honour it. A
// caller that depends on the memory being there should check it. Live against
// a backend with no live-snapshot support is a 501 unimplemented, not a
// conflict.
func (s *SnapshotsService) Create(ctx context.Context, vmID string, reqBody SnapshotRequest) (*Snapshot, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("snapshots service is not configured")
	}
	if vmID == "" {
		return nil, errors.New("vm id is required")
	}
	path := "/v1/environments/" + url.PathEscape(vmID) + "/snapshots"
	req, err := s.t.newRequest(ctx, http.MethodPost, path, nil, reqBody)
	if err != nil {
		return nil, err
	}
	resp, err := s.t.do(req)
	if err != nil {
		return nil, err
	}
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var snap Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	return &snap, nil
}

// List returns every snapshot matching opt, transparently walking every
// result page. For explicit single-page control (e.g. a CLI --cursor
// flag), use ListPage.
func (s *SnapshotsService) List(ctx context.Context, opt ListSnapshotsOptions) ([]Snapshot, error) {
	var out []Snapshot
	cursor := opt.Cursor
	for {
		page, err := s.ListPage(ctx, ListSnapshotsOptions{
			VMID:     opt.VMID,
			TaskID:   opt.TaskID,
			TenantID: opt.TenantID,
			State:    opt.State,
			Mode:     opt.Mode,
			Name:     opt.Name,
			LayerKey: opt.LayerKey,
			Arch:     opt.Arch,
			Limit:    maxPageLimit,
			Cursor:   cursor,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Snapshots...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// ListPage returns one page of snapshots matching opt.
func (s *SnapshotsService) ListPage(ctx context.Context, opt ListSnapshotsOptions) (*SnapshotPage, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("snapshots service is not configured")
	}
	values := url.Values{}
	if opt.VMID != "" {
		values.Set("vm_id", opt.VMID)
	}
	if opt.TaskID != "" {
		values.Set("task_id", opt.TaskID)
	}
	if opt.TenantID != "" {
		values.Set("tenant_id", opt.TenantID)
	}
	if opt.State != "" {
		values.Set("state", opt.State)
	}
	if opt.Mode != "" {
		values.Set("mode", opt.Mode)
	}
	if opt.Name != "" {
		values.Set("name", opt.Name)
	}
	if opt.LayerKey != "" {
		values.Set("layer_key", opt.LayerKey)
	}
	if opt.Arch != "" {
		values.Set("arch", opt.Arch)
	}
	setPaginationParams(values, opt.Limit, opt.Cursor)
	req, err := s.t.newRequest(ctx, http.MethodGet, "/v1/snapshots", values, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.t.do(req)
	if err != nil {
		return nil, err
	}
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out snapshotList
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode snapshots: %w", err)
	}
	page := &SnapshotPage{Snapshots: out.Snapshots}
	if out.NextCursor != nil {
		page.NextCursor = *out.NextCursor
	}
	return page, nil
}

// Resolve looks up the newest ready build artifact for one layer cache key and
// architecture, reporting ok=false when there is none.
//
// A miss is not an error. A cold cache is the normal state of a first build, and
// modelling it as a failure would make every caller wrap this in error handling
// to discover nothing was wrong.
//
// arch is required rather than defaulted. An ext4 rootfs is not portable across
// architectures, so resolving without one could hand back an artifact the caller
// cannot boot, at the exact moment it believes it got a cache hit.
//
// The scope searched comes from how the client authenticated. There is
// deliberately no tenant parameter: a caller that could name its own scope could
// read another tenant's cache.
func (s *SnapshotsService) Resolve(ctx context.Context, layerKey, arch string) (*Snapshot, bool, error) {
	if s == nil || s.t == nil {
		return nil, false, errors.New("snapshots service is not configured")
	}
	if layerKey == "" {
		return nil, false, errors.New("layer key is required")
	}
	if arch == "" {
		return nil, false, errors.New("arch is required")
	}
	values := url.Values{}
	values.Set("layer_key", layerKey)
	values.Set("arch", arch)
	req, err := s.t.newRequest(ctx, http.MethodGet, "/v1/snapshots/resolve", values, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := s.t.do(req)
	if err != nil {
		return nil, false, err
	}
	if err := CheckResponse(resp); err != nil {
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out resolveSnapshotResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false, fmt.Errorf("decode resolved snapshot: %w", err)
	}
	if !out.Found || out.Snapshot == nil {
		return nil, false, nil
	}
	return out.Snapshot, true, nil
}

func (s *SnapshotsService) Get(ctx context.Context, snapshotID string) (*Snapshot, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("snapshots service is not configured")
	}
	if snapshotID == "" {
		return nil, errors.New("snapshot id is required")
	}
	path := "/v1/snapshots/" + url.PathEscape(snapshotID)
	req, err := s.t.newRequest(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.t.do(req)
	if err != nil {
		return nil, err
	}
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var snap Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	return &snap, nil
}

func (s *SnapshotsService) Delete(ctx context.Context, snapshotID string) error {
	if s == nil || s.t == nil {
		return errors.New("snapshots service is not configured")
	}
	if snapshotID == "" {
		return errors.New("snapshot id is required")
	}
	path := "/v1/snapshots/" + url.PathEscape(snapshotID)
	req, err := s.t.newRequest(ctx, http.MethodDelete, path, nil, nil)
	if err != nil {
		return err
	}
	resp, err := s.t.do(req)
	if err != nil {
		return err
	}
	if err := CheckResponse(resp); err != nil {
		return err
	}
	return nil
}

func (s *SnapshotsService) Restore(ctx context.Context, snapshotID string) error {
	if s == nil || s.t == nil {
		return errors.New("snapshots service is not configured")
	}
	if snapshotID == "" {
		return errors.New("snapshot id is required")
	}
	path := "/v1/snapshots/" + url.PathEscape(snapshotID)
	values := url.Values{}
	values.Set("action", "restore")
	req, err := s.t.newRequest(ctx, http.MethodPost, path, values, nil)
	if err != nil {
		return err
	}
	resp, err := s.t.do(req)
	if err != nil {
		return err
	}
	if err := CheckResponse(resp); err != nil {
		return err
	}
	return nil
}
