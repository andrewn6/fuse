package fuse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type transport struct {
	baseURL *url.URL
	http    *http.Client
	// streamHTTP is a no-timeout client used for long-lived SSE
	// streams (events). do not use it for normal requests.
	streamHTTP *http.Client
	bearer     string
	userAgent  string
	requestID  func() string
}

func (t *transport) newRequest(ctx context.Context, method, path string, query url.Values, body any) (*http.Request, error) {
	if t == nil {
		return nil, errors.New("transport is nil")
	}
	if ctx == nil {
		return nil, errors.New("context is nil")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	ref, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("parse path: %w", err)
	}
	if len(query) > 0 {
		ref.RawQuery = query.Encode()
	}
	var buf *bytes.Buffer
	if body != nil {
		buf = &bytes.Buffer{}
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(body); err != nil {
			return nil, fmt.Errorf("encode body: %w", err)
		}
	}
	base := &url.URL{}
	if t.baseURL != nil {
		base = t.baseURL
	}
	u := base.ResolveReference(ref)
	// use an io.Reader interface var, not a *bytes.Reader, so a
	// missing body is an untyped nil. passing a typed nil *bytes.Reader
	// makes net/http panic when it type-asserts and calls Len().
	var reader io.Reader
	if buf != nil {
		reader = bytes.NewReader(buf.Bytes())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if t.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+t.bearer)
	}
	if t.userAgent != "" {
		req.Header.Set("User-Agent", t.userAgent)
	}
	if t.requestID != nil {
		if id := t.requestID(); id != "" {
			req.Header.Set(requestIDHeader, id)
		}
	}
	return req, nil
}

func (t *transport) do(req *http.Request) (*http.Response, error) {
	if t == nil {
		return nil, errors.New("transport is nil")
	}
	client := t.http
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	return resp, nil
}

// doStream runs a request on the no-timeout client, for calls whose duration is
// bounded by the request context rather than a fixed client timeout (a
// long-running exec, an SSE stream).
func (t *transport) doStream(req *http.Request) (*http.Response, error) {
	if t == nil {
		return nil, errors.New("transport is nil")
	}
	client := t.streamHTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	return resp, nil
}

type EnvironmentsService struct {
	t *transport
}

func newEnvironmentsService(t *transport) *EnvironmentsService {
	return &EnvironmentsService{t: t}
}

type ListEnvironmentsOptions struct {
	TaskID string
	State  string
	HostID string

	// Limit caps the page size (server default 50, max 200; <=0 uses the
	// server default). Only consulted by ListPage — List always requests
	// the server's max page size internally since it walks every page.
	Limit int

	// Cursor resumes after a previous page's EnvironmentPage.NextCursor. It
	// is an opaque token; treat it as such rather than parsing it.
	Cursor string
}

// EnvironmentPage is one page of a List call, plus the cursor to fetch the
// next one.
type EnvironmentPage struct {
	Environments []EnvironmentInfo `json:"environments"`
	// NextCursor is empty once there are no more results.
	NextCursor string `json:"next_cursor,omitempty"`
}

// List returns every environment matching opt, transparently walking every
// result page. For explicit single-page control (e.g. a CLI --cursor
// flag), use ListPage.
func (s *EnvironmentsService) List(ctx context.Context, opt ListEnvironmentsOptions) ([]EnvironmentInfo, error) {
	var out []EnvironmentInfo
	cursor := opt.Cursor
	for {
		page, err := s.ListPage(ctx, ListEnvironmentsOptions{
			TaskID: opt.TaskID,
			State:  opt.State,
			HostID: opt.HostID,
			Limit:  maxPageLimit,
			Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Environments...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// ListPage returns one page of environments matching opt.
func (s *EnvironmentsService) ListPage(ctx context.Context, opt ListEnvironmentsOptions) (*EnvironmentPage, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("environments service is not configured")
	}
	values := url.Values{}
	if opt.TaskID != "" {
		values.Set("task_id", opt.TaskID)
	}
	if opt.State != "" {
		values.Set("state", opt.State)
	}
	if opt.HostID != "" {
		values.Set("host_id", opt.HostID)
	}
	setPaginationParams(values, opt.Limit, opt.Cursor)
	req, err := s.t.newRequest(ctx, http.MethodGet, "/v1/environments", values, nil)
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
	var out environmentList
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode environments: %w", err)
	}
	page := &EnvironmentPage{Environments: out.Environments}
	if out.NextCursor != nil {
		page.NextCursor = *out.NextCursor
	}
	return page, nil
}

func (s *EnvironmentsService) Get(ctx context.Context, vmID string) (*EnvironmentInfo, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("environments service is not configured")
	}
	if vmID == "" {
		return nil, errors.New("vm id is required")
	}
	path := "/v1/environments/" + url.PathEscape(vmID)
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
	var env EnvironmentInfo
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode environment: %w", err)
	}
	return &env, nil
}

func (s *EnvironmentsService) Create(ctx context.Context, reqBody CreateRequest) (*EnvironmentInfo, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("environments service is not configured")
	}
	req, err := s.t.newRequest(ctx, http.MethodPost, "/v1/environments", nil, reqBody)
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
	var env EnvironmentInfo
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode environment: %w", err)
	}
	return &env, nil
}

func (s *EnvironmentsService) Drain(ctx context.Context, vmID string) (*EnvironmentInfo, error) {
	return s.action(ctx, vmID, "drain")
}

// Fork creates a new environment seeded from an existing one and returns the
// new environment. when opts.ReuseSnapshotID is empty the server snapshots the
// source first; otherwise it reuses the named snapshot. it does not use the
// shared action helper because fork sends a request body.
func (s *EnvironmentsService) Fork(ctx context.Context, vmID string, opts ForkOptions) (*EnvironmentInfo, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("environments service is not configured")
	}
	if vmID == "" {
		return nil, errors.New("vm id is required")
	}
	path := "/v1/environments/" + url.PathEscape(vmID)
	values := url.Values{}
	values.Set("action", "fork")
	req, err := s.t.newRequest(ctx, http.MethodPost, path, values, opts)
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
	var env EnvironmentInfo
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode environment: %w", err)
	}
	return &env, nil
}

func (s *EnvironmentsService) RotateToken(ctx context.Context, vmID string) error {
	if s == nil || s.t == nil {
		return errors.New("environments service is not configured")
	}
	if vmID == "" {
		return errors.New("vm id is required")
	}
	path := "/v1/environments/" + url.PathEscape(vmID)
	values := url.Values{}
	values.Set("action", "rotate-token")
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

func (s *EnvironmentsService) Destroy(ctx context.Context, vmID string) error {
	if s == nil || s.t == nil {
		return errors.New("environments service is not configured")
	}
	if vmID == "" {
		return errors.New("vm id is required")
	}
	path := "/v1/environments/" + url.PathEscape(vmID)
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

func (s *EnvironmentsService) action(ctx context.Context, vmID, action string) (*EnvironmentInfo, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("environments service is not configured")
	}
	if vmID == "" {
		return nil, errors.New("vm id is required")
	}
	if action == "" {
		return nil, errors.New("action is required")
	}
	path := "/v1/environments/" + url.PathEscape(vmID)
	values := url.Values{}
	values.Set("action", action)
	req, err := s.t.newRequest(ctx, http.MethodPost, path, values, nil)
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
	var env EnvironmentInfo
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode environment: %w", err)
	}
	return &env, nil
}
