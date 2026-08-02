package fuse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

type HostsService struct {
	t *transport
}

func newHostsService(t *transport) *HostsService {
	return &HostsService{t: t}
}

func (s *HostsService) Register(ctx context.Context, req RegisterHostRequest) (*Host, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("hosts service is not configured")
	}
	httpReq, err := s.t.newRequest(ctx, http.MethodPost, "/v1/hosts", nil, req)
	if err != nil {
		return nil, err
	}
	resp, err := s.t.do(httpReq)
	if err != nil {
		return nil, err
	}
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var host Host
	if err := json.NewDecoder(resp.Body).Decode(&host); err != nil {
		return nil, fmt.Errorf("decode host: %w", err)
	}
	return &host, nil
}

// ListHostsOptions controls pagination for HostsService.ListPage.
type ListHostsOptions struct {
	// Limit caps the page size (server default 50, max 200; <=0 uses the
	// server default).
	Limit int

	// Cursor resumes after a previous page's HostPage.NextCursor. It is an
	// opaque token; treat it as such rather than parsing it.
	Cursor string
}

// HostPage is one page of a List call, plus the cursor to fetch the next
// one.
type HostPage struct {
	Hosts []Host `json:"hosts"`
	// NextCursor is empty once there are no more results.
	NextCursor string `json:"next_cursor,omitempty"`
}

// List returns every registered host, transparently walking every result
// page. For explicit single-page control (e.g. a CLI --cursor flag), use
// ListPage.
func (s *HostsService) List(ctx context.Context) ([]Host, error) {
	var out []Host
	cursor := ""
	for {
		page, err := s.ListPage(ctx, ListHostsOptions{Limit: maxPageLimit, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Hosts...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// ListPage returns one page of registered hosts.
func (s *HostsService) ListPage(ctx context.Context, opt ListHostsOptions) (*HostPage, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("hosts service is not configured")
	}
	values := url.Values{}
	setPaginationParams(values, opt.Limit, opt.Cursor)
	req, err := s.t.newRequest(ctx, http.MethodGet, "/v1/hosts", values, nil)
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
	var out hostList
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode hosts: %w", err)
	}
	page := &HostPage{Hosts: out.Hosts}
	if out.NextCursor != nil {
		page.NextCursor = *out.NextCursor
	}
	return page, nil
}

func (s *HostsService) Get(ctx context.Context, hostID string) (*Host, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("hosts service is not configured")
	}
	if hostID == "" {
		return nil, errors.New("host id is required")
	}
	path := "/v1/hosts/" + url.PathEscape(hostID)
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
	var host Host
	if err := json.NewDecoder(resp.Body).Decode(&host); err != nil {
		return nil, fmt.Errorf("decode host: %w", err)
	}
	return &host, nil
}

func (s *HostsService) Cordon(ctx context.Context, hostID string) error {
	return s.action(ctx, hostID, "cordon")
}

func (s *HostsService) Uncordon(ctx context.Context, hostID string) error {
	return s.action(ctx, hostID, "uncordon")
}

func (s *HostsService) Deregister(ctx context.Context, hostID string) error {
	if s == nil || s.t == nil {
		return errors.New("hosts service is not configured")
	}
	if hostID == "" {
		return errors.New("host id is required")
	}
	path := "/v1/hosts/" + url.PathEscape(hostID)
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

func (s *HostsService) action(ctx context.Context, hostID, action string) error {
	if s == nil || s.t == nil {
		return errors.New("hosts service is not configured")
	}
	if hostID == "" {
		return errors.New("host id is required")
	}
	if action == "" {
		return errors.New("action is required")
	}
	path := "/v1/hosts/" + url.PathEscape(hostID)
	values := url.Values{}
	values.Set("action", action)
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
