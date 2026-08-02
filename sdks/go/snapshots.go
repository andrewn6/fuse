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
