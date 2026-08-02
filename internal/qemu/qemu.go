// Package qemu implements a QEMU/KVM-based Provider for GPU passthrough
// environments. It mirrors the firecracker package: a remote HTTP host-agent
// client plus an in-memory stub fallback for dev/tests so Fleet/Boot stay
// provider-agnostic.
//
// The QEMU Environment deliberately does NOT implement
// orchestrator.SnapshotCapable: a VFIO-passed-through GPU cannot be
// checkpointed, so snapshot and fork must fail cleanly (see decision D4). The
// missing methods are what the orchestrator's SnapshotCapable/SnapshotForkable
// type assertions key off, so no core changes are needed.
package qemu

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/folsomintel/fuse/internal/hostwire"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// fused guest paths for the host-agent start-agent wire. The qemu host agent
// (PR 4) speaks the same contract as fc-agent.py, so these mirror the
// firecracker package's frozen-wire path constants. This package cannot import
// the orchestrator's unexported consts, hence the duplication by design.
const (
	fusedManifestGuestPath = "/fuse/manifest.json"
	fusedSecretsGuestPath  = "/fuse/secrets.json"
	fusedTLSCertGuestPath  = "/fuse/tls/cert.pem"
	fusedTLSKeyGuestPath   = "/fuse/tls/key.pem"
)

// Config configures the QEMU provider client.
type Config struct {
	BaseURL     string       // host agent base URL, e.g. https://gpu-host.local
	Token       string       // bearer token for auth
	HTTPClient  *http.Client // optional; defaults to http.DefaultClient
	UseStub     bool         // force in-memory stub (for dev/tests)
	DownloadURL string       // optional URL to fetch the guest agent binary
}

// Provider implements orchestrator.Provider backed by a QEMU host agent API.
// If UseStub is true or BaseURL is empty, it falls back to an in-memory stub
// that simulates behavior without real VMs.
type Provider struct {
	baseURL     string
	token       string
	client      *http.Client
	downloadURL string

	stub *stubProvider
}

// New creates a QEMU provider.
//
// The default HTTP client comes from hostwire: dedicated dial, TLS,
// response-header, and overall timeouts, and a redirect policy that refuses to
// follow a host agent somewhere else. http.DefaultClient has no timeout, so an
// unresponsive host would hold a provider call open indefinitely.
func New(cfg Config) *Provider {
	p := &Provider{
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		token:       cfg.Token,
		client:      cfg.HTTPClient,
		downloadURL: cfg.DownloadURL,
	}
	if p.client == nil {
		p.client = hostwire.NewClient()
	}
	if cfg.UseStub || p.baseURL == "" {
		p.stub = newStub()
	}
	return p
}

var _ orchestrator.Provider = (*Provider)(nil)

// Create provisions a new sandbox. When spec.GPUs > 0, the GPU count and kind
// are forwarded to the host agent so it can attach VFIO devices; GPUs == 0
// omits them (task 3.2).
func (p *Provider) Create(ctx context.Context, spec orchestrator.Spec) (orchestrator.Environment, error) {
	if p.stub != nil {
		return p.stub.Create(ctx, spec)
	}

	reqBody := createVMRequest{
		Name:             spec.Name,
		CPUs:             spec.CPUs,
		MemoryMB:         spec.RamMB,
		StorageGB:        spec.StorageGB,
		Region:           spec.Region,
		Image:            spec.Image,
		GPUs:             spec.GPUs,
		GPUKind:          spec.GPUKind,
		GPUProfile:       spec.GPUProfile,
		MIGInstanceUUIDs: spec.MIGInstanceUUIDs,
	}
	var resp createVMResponse
	if err := p.doJSON(ctx, http.MethodPost, "/v1/vm", reqBody, &resp); err != nil {
		return nil, fmt.Errorf("qemu create vm: %w", err)
	}

	env := &remoteEnv{id: resp.VMID, url: resp.URL, client: p}
	if env.url == "" {
		env.url = fmt.Sprintf("qemu://%s", resp.VMID)
	}
	return env, nil
}

// Get returns an existing sandbox.
func (p *Provider) Get(ctx context.Context, name string) (orchestrator.Environment, error) {
	if p.stub != nil {
		return p.stub.Get(ctx, name)
	}
	path := fmt.Sprintf("/v1/vm/%s", name)
	var resp getVMResponse
	if err := p.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("qemu get vm: %w", err)
	}
	return &remoteEnv{id: resp.VMID, url: resp.URL, client: p}, nil
}

// Destroy tears down a sandbox.
func (p *Provider) Destroy(ctx context.Context, name string) error {
	if p.stub != nil {
		return p.stub.Destroy(ctx, name)
	}
	path := fmt.Sprintf("/v1/vm/%s", name)
	return p.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// List returns all sandboxes matching the given prefix.
func (p *Provider) List(ctx context.Context, prefix string) ([]orchestrator.Environment, error) {
	if p.stub != nil {
		return p.stub.List(ctx, prefix)
	}
	path := "/v1/vm"
	if prefix != "" {
		path += "?prefix=" + prefix
	}
	var resp listVMResponse
	if err := p.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("qemu list vms: %w", err)
	}
	envs := make([]orchestrator.Environment, 0, len(resp.VMs))
	for _, vm := range resp.VMs {
		envs = append(envs, &remoteEnv{id: vm.VMID, url: vm.URL, client: p})
	}
	return envs, nil
}

// Close releases resources.
func (p *Provider) Close() error {
	if p.stub != nil {
		return p.stub.Close()
	}
	return nil
}

// Capacity implements orchestrator.CapacityProber by asking the host agent
// for its real CPU count, total RAM, and free disk via GET /v1/capacity.
// Not supported in stub mode: there is no real hardware for the in-memory
// stub to report on.
func (p *Provider) Capacity(ctx context.Context) (orchestrator.HostCapacity, error) {
	if p.stub != nil {
		return orchestrator.HostCapacity{}, errors.New("qemu: capacity probing requires a real host agent, not the in-memory stub")
	}
	var resp capacityResponse
	if err := p.doJSON(ctx, http.MethodGet, "/v1/capacity", nil, &resp); err != nil {
		return orchestrator.HostCapacity{}, fmt.Errorf("qemu capacity: %w", err)
	}
	return orchestrator.HostCapacity{
		CPUs:         resp.CPUs,
		RamMB:        resp.RamMB,
		StorageGB:    resp.StorageGB,
		Arch:         resp.Arch,
		GPUs:         resp.GPUs,
		GPUKind:      resp.GPUKind,
		GPUDevices:   resp.GPUDevices,
		MIGProfiles:  resp.MIGProfiles,
		MIGInstances: resp.MIGInstances,
	}, nil
}

var _ orchestrator.CapacityProber = (*Provider)(nil)

// remoteEnv represents a QEMU VM managed by the host agent. It implements
// orchestrator.Environment (plus TokenSetter) but NOT SnapshotCapable: a
// passed-through GPU cannot be checkpointed (D4).
type remoteEnv struct {
	id     string
	url    string
	client *Provider

	tokenMu   sync.RWMutex
	authToken string

	endpointsMu sync.RWMutex
	endpoints   []orchestrator.Endpoint
}

var (
	_ orchestrator.Environment      = (*remoteEnv)(nil)
	_ orchestrator.Attacher         = (*remoteEnv)(nil)
	_ orchestrator.TokenSetter      = (*remoteEnv)(nil)
	_ orchestrator.EndpointReporter = (*remoteEnv)(nil)
)

func (e *remoteEnv) Endpoints() []orchestrator.Endpoint {
	e.endpointsMu.RLock()
	defer e.endpointsMu.RUnlock()
	return append([]orchestrator.Endpoint(nil), e.endpoints...)
}

func (e *remoteEnv) Name() string { return e.id }
func (e *remoteEnv) URL() string  { return e.url }

// Token returns the per-VM auth token previously installed via SetToken.
func (e *remoteEnv) Token() string {
	e.tokenMu.RLock()
	defer e.tokenMu.RUnlock()
	return e.authToken
}

// SetToken records the per-VM auth token so subsequent Token() calls return it.
func (e *remoteEnv) SetToken(token string) {
	e.tokenMu.Lock()
	e.authToken = token
	e.tokenMu.Unlock()
}

// Exec runs argv in the guest via the host agent and returns the result with
// stdout, stderr, and the exit code intact. A non-zero exit code is a normal
// outcome, not an error: the guest ran the command and it failed, which is
// information the caller asked for.
func (e *remoteEnv) Exec(ctx context.Context, cmd []string, opts orchestrator.ExecOptions) (orchestrator.ExecResult, error) {
	req := execRequest{Cmd: cmd}
	if opts.Timeout > 0 {
		req.TimeoutMS = int(opts.Timeout.Milliseconds())
	}
	var resp execResponse
	if err := e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/exec", e.id), req, &resp); err != nil {
		return orchestrator.ExecResult{}, fmt.Errorf("exec: %w", err)
	}
	return orchestrator.ExecResult{
		ExitCode: resp.ExitCode,
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
	}, nil
}

// Attach opens a fuse-attach/1 stream to a process in the guest. The frames on
// it are opaque here: the provider is a conduit, and only the far ends (the
// host agent and the client) encode or decode them.
func (e *remoteEnv) Attach(ctx context.Context, spec orchestrator.AttachSpec) (io.ReadWriteCloser, error) {
	path := fmt.Sprintf("/v1/vm/%s/attach?%s", e.id, hostwire.AttachQuery(spec).Encode())
	c, err := hostwire.Dial(ctx, e.client.baseURL, e.client.token, path, hostwire.AttachProto)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (e *remoteEnv) ExecStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	req := execRequest{Cmd: append([]string{name}, args...)}
	var resp execResponse
	if err := e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/exec", e.id), req, &resp); err != nil {
		return fmt.Errorf("exec stream: %w", err)
	}
	if len(resp.Stdout) > 0 {
		_, _ = stdout.Write(resp.Stdout)
	}
	if len(resp.Stderr) > 0 {
		_, _ = stderr.Write(resp.Stderr)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("exec exit %d", resp.ExitCode)
	}
	return nil
}

func (e *remoteEnv) Upload(ctx context.Context, data []byte, path string) error {
	req := uploadRequest{Path: path, ContentB64: base64.StdEncoding.EncodeToString(data)}
	return e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/upload", e.id), req, nil)
}

// StartAgent launches the guest agent via the host agent's /start-agent wire,
// mirroring the firecracker contract (the qemu host agent in PR 4 implements
// the same endpoint). The host agent owns the launch mechanism, so only the
// structured fused guest paths and generic spec fields pass through.
func (e *remoteEnv) StartAgent(ctx context.Context, spec orchestrator.AgentSpec) error {
	downloadURL := spec.DownloadURL
	if downloadURL == "" {
		downloadURL = e.client.downloadURL
	}
	agentReq := startAgentRequest{
		ManifestPath: fusedManifestGuestPath,
		SecretsPath:  fusedSecretsGuestPath,
		TLSCertPath:  fusedTLSCertGuestPath,
		TLSKeyPath:   fusedTLSKeyGuestPath,
		AuthToken:    spec.AuthToken,
		Gateway:      spec.Gateway,
		GatewayToken: spec.GatewayToken,
		DownloadURL:  downloadURL,
		Expose:       toWireExpose(spec.Expose),
	}
	var resp startAgentResponse
	if err := e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/start-agent", e.id), agentReq, &resp); err != nil {
		return err
	}
	endpoints := make([]orchestrator.Endpoint, len(resp.Endpoints))
	for i, endpoint := range resp.Endpoints {
		endpoints[i] = orchestrator.Endpoint{As: endpoint.As, URL: endpoint.URL, Port: endpoint.Port}
	}
	e.endpointsMu.Lock()
	e.endpoints = endpoints
	e.endpointsMu.Unlock()
	return nil
}

// HTTP helpers and request/response shapes.

type createVMRequest struct {
	Name      string `json:"name"`
	CPUs      int    `json:"cpus"`
	MemoryMB  int    `json:"memory_mb"`
	StorageGB int    `json:"storage_gb"`
	Region    string `json:"region"`
	Image     string `json:"image,omitempty"`
	GPUs      int32  `json:"gpus,omitempty"`
	GPUKind   string `json:"gpu_kind,omitempty"`
	// GPUProfile requests MIG instances instead of whole devices: the host
	// agent allocates mdev devices of this profile and attaches them via
	// vfio-pci sysfsdev (decision D5).
	GPUProfile string `json:"gpu_profile,omitempty"`
	// MIGInstanceUUIDs are the concrete MIG instance uuids the orchestrator's
	// per-instance scheduler chose for this VM. when set, the agent binds
	// exactly these (validating them against its inventory) instead of
	// picking locally. empty on count-map hosts (issue #41).
	MIGInstanceUUIDs []string `json:"mig_instance_uuids,omitempty"`
}

type createVMResponse struct {
	VMID string `json:"vm_id"`
	URL  string `json:"url"`
}

type getVMResponse struct {
	VMID string `json:"vm_id"`
	URL  string `json:"url"`
}

// capacityResponse is the GET /v1/capacity response: the host agent's real
// CPU count, total RAM, free disk, and probed GPU inventory.
type capacityResponse struct {
	CPUs         int                        `json:"cpus"`
	RamMB        int                        `json:"ram_mb"`
	StorageGB    int                        `json:"storage_gb"`
	Arch         string                     `json:"arch"`
	GPUs         int                        `json:"gpus"`
	GPUKind      string                     `json:"gpu_kind"`
	GPUDevices   []orchestrator.GPUDevice   `json:"gpu_devices"`
	MIGProfiles  map[string]int             `json:"mig_profiles,omitempty"`
	MIGInstances []orchestrator.MIGInstance `json:"mig_instances,omitempty"`
}

type listVMResponse struct {
	VMs []struct {
		VMID string `json:"vm_id"`
		URL  string `json:"url"`
	} `json:"vms"`
}

type uploadRequest struct {
	Path       string `json:"path"`
	ContentB64 string `json:"content_b64"`
}

type execRequest struct {
	Cmd []string `json:"cmd"`
	// TimeoutMS bounds the command in the guest. Zero leaves the host agent
	// on its own default.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

type execResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
}

type startAgentRequest struct {
	ManifestPath string       `json:"manifest_path"`
	SecretsPath  string       `json:"secrets_path"`
	TLSCertPath  string       `json:"tls_cert_path,omitempty"`
	TLSKeyPath   string       `json:"tls_key_path,omitempty"`
	AuthToken    string       `json:"auth_token,omitempty"`
	Gateway      string       `json:"gateway,omitempty"`
	GatewayToken string       `json:"gateway_token,omitempty"`
	DownloadURL  string       `json:"download_url,omitempty"`
	Expose       []exposeWire `json:"expose,omitempty"`
}

type exposeWire struct {
	Port int    `json:"port"`
	As   string `json:"as,omitempty"`
}

type endpointWire struct {
	As   string `json:"as,omitempty"`
	URL  string `json:"url"`
	Port int    `json:"port"`
}

type startAgentResponse struct {
	Endpoints []endpointWire `json:"endpoints,omitempty"`
}

func toWireExpose(in []orchestrator.ExposeSpec) []exposeWire {
	if len(in) == 0 {
		return nil
	}
	out := make([]exposeWire, len(in))
	for i, expose := range in {
		out[i] = exposeWire{Port: expose.Port, As: expose.As}
	}
	return out
}

func (p *Provider) doJSON(ctx context.Context, method, path string, reqBody any, respBody any) error {
	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}

	res, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode >= 300 {
		// Bounded: an agent error body is diagnostic text, and reading an
		// unbounded one into an error string is a way to make the
		// orchestrator allocate on someone else's terms.
		return &orchestrator.HTTPStatusError{Code: res.StatusCode, Body: hostwire.ReadErrorBody(res.Body)}
	}
	if respBody == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(respBody)
}

// stubProvider is the in-memory fallback used when BaseURL is empty or UseStub
// is set. It records the last create spec per env so tests can assert GPU
// forwarding without a real host agent.
type stubProvider struct {
	mu   sync.Mutex
	envs map[string]*stubEnv
}

func newStub() *stubProvider {
	return &stubProvider{envs: make(map[string]*stubEnv)}
}

func (p *stubProvider) Create(_ context.Context, spec orchestrator.Spec) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.envs[spec.Name]; exists {
		return nil, fmt.Errorf("env %s already exists", spec.Name)
	}
	env := &stubEnv{
		name:       spec.Name,
		url:        fmt.Sprintf("qemu://%s", spec.Name),
		gpus:       spec.GPUs,
		gpuKind:    spec.GPUKind,
		gpuProfile: spec.GPUProfile,
	}
	p.envs[spec.Name] = env
	return env, nil
}

func (p *stubProvider) Get(_ context.Context, name string) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	env, ok := p.envs[name]
	if !ok {
		return nil, fmt.Errorf("env %s not found", name)
	}
	return env, nil
}

func (p *stubProvider) Destroy(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.envs, name)
	return nil
}

func (p *stubProvider) List(_ context.Context, prefix string) ([]orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	envs := make([]orchestrator.Environment, 0, len(p.envs))
	for name, env := range p.envs {
		if prefix == "" || strings.HasPrefix(name, prefix) {
			envs = append(envs, env)
		}
	}
	return envs, nil
}

func (p *stubProvider) Close() error { return nil }

// stubEnv simulates a QEMU VM in memory. Like remoteEnv, it implements
// Environment but NOT SnapshotCapable, so snapshot/fork guardrails are
// exercised identically in stub-backed tests.
type stubEnv struct {
	name       string
	url        string
	gpus       int32  // spec.GPUs at Create time, kept for test inspection
	gpuKind    string // spec.GPUKind at Create time, kept for test inspection
	gpuProfile string // spec.GPUProfile at Create time, kept for test inspection

	mu        sync.Mutex
	files     map[string][]byte
	authToken string
	endpoints []orchestrator.Endpoint
}

var (
	_ orchestrator.Environment      = (*stubEnv)(nil)
	_ orchestrator.TokenSetter      = (*stubEnv)(nil)
	_ orchestrator.EndpointReporter = (*stubEnv)(nil)
)

func (e *stubEnv) Endpoints() []orchestrator.Endpoint {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]orchestrator.Endpoint(nil), e.endpoints...)
}

func (e *stubEnv) Name() string { return e.name }
func (e *stubEnv) URL() string  { return e.url }

func (e *stubEnv) Token() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.authToken
}

func (e *stubEnv) SetToken(token string) {
	e.mu.Lock()
	e.authToken = token
	e.mu.Unlock()
}

// Exec reports that the stub has no guest to run commands in. The stub used
// to fake a successful empty run, which meant a provider with an unset BaseURL
// answered every exec with a convincing lie. A caller debugging a VM needs to
// know it is talking to nothing.
func (e *stubEnv) Exec(_ context.Context, _ []string, _ orchestrator.ExecOptions) (orchestrator.ExecResult, error) {
	return orchestrator.ExecResult{}, orchestrator.ErrExecUnsupported
}

// ExecStream simulates a successful guest command (no output written).
func (e *stubEnv) ExecStream(_ context.Context, _, _ io.Writer, _ string, _ ...string) error {
	return nil
}

func (e *stubEnv) Upload(_ context.Context, data []byte, path string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.files == nil {
		e.files = make(map[string][]byte)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	e.files[path] = cp
	return nil
}

func (e *stubEnv) StartAgent(_ context.Context, spec orchestrator.AgentSpec) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.endpoints = make([]orchestrator.Endpoint, len(spec.Expose))
	for i, expose := range spec.Expose {
		e.endpoints[i] = orchestrator.Endpoint{As: expose.As, URL: e.url, Port: expose.Port}
	}
	return nil
}
