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

func (s *SnapshotsService) List(ctx context.Context, opt ListSnapshotsOptions) ([]Snapshot, error) {
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
	return out.Snapshots, nil
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
