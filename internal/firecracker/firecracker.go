// Package firecracker implements a Firecracker-based Provider. It supports a
// remote HTTP host-agent client and a stub in-memory fallback for dev/tests so
// Fleet/Boot can remain provider-agnostic.
package firecracker

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
	"time"

	"github.com/folsomintel/fuse/internal/hostwire"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// fused guest paths for the FROZEN host-agent /start-surfd wire. The external
// firecracker host agent cannot be changed, so its request still carries the
// structured manifest/secrets/TLS guest paths. These constants exist solely to
// populate that frozen wire and mirror the core fused profile's path constants
// (see orchestrator/agent_profile.go). The firecracker package cannot import
// the orchestrator's unexported consts, hence the duplication by design.
const (
	fusedManifestGuestPath = "/fuse/manifest.json"
	fusedSecretsGuestPath  = "/fuse/secrets.json"
	fusedTLSCertGuestPath  = "/fuse/tls/cert.pem"
	fusedTLSKeyGuestPath   = "/fuse/tls/key.pem"
)

// Config configures the Firecracker provider client.
type Config struct {
	BaseURL     string       // host agent base URL, e.g. https://agent.local
	Token       string       // bearer token for auth
	HTTPClient  *http.Client // optional; defaults to http.DefaultClient
	UseStub     bool         // force in-memory stub (for dev/tests)
	DownloadURL string       // optional URL to fetch the guest agent binary (forwarded to /start-agent)
}

// Provider implements orchestrator.Provider backed by a Firecracker host agent
// API. If UseStub is true or BaseURL is empty, it falls back to an in-memory
// stub that simulates behavior without real microVMs.
type Provider struct {
	baseURL     string
	token       string
	client      *http.Client
	downloadURL string

	stub *stubProvider
}

// New creates a Firecracker provider.
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

var (
	_ orchestrator.Provider         = (*Provider)(nil)
	_ orchestrator.SnapshotForkable = (*Provider)(nil)
)

// Create provisions a new sandbox.
func (p *Provider) Create(ctx context.Context, spec orchestrator.Spec) (orchestrator.Environment, error) {
	if p.stub != nil {
		return p.stub.Create(ctx, spec)
	}

	reqBody := createVMRequest{
		Name:         spec.Name,
		CPUs:         spec.CPUs,
		MemoryMB:     spec.RamMB,
		StorageGB:    spec.StorageGB,
		Region:       spec.Region,
		Image:        spec.Image,
		SeedSnapshot: spec.SeedSnapshotID,
	}
	var resp createVMResponse
	if err := p.doJSON(ctx, http.MethodPost, "/v1/vm", reqBody, &resp); err != nil {
		return nil, fmt.Errorf("firecracker create vm: %w", err)
	}

	env := &remoteEnv{
		id:     resp.VMID,
		url:    resp.URL,
		client: p,
	}
	if env.url == "" {
		env.url = fmt.Sprintf("fc://%s", resp.VMID)
	}
	return env, nil
}

// CreateFromCheckpoint provisions a brand-new sandbox seeded from an existing
// vm's checkpoint, satisfying orchestrator.SnapshotForkable. The host agent
// copies the named snapshot's rootfs into the new vm rather than the base
// image; the source vm is untouched and keeps running.
//
// The returned Environment is a fully usable handle for the NEW vm (its own id
// and url), not a view of the source. It carries no auth token: the guest's
// disk still holds whatever credentials the source had, so ForkEnvironment
// mints fresh ones for the fork rather than letting two vms share a secret.
func (p *Provider) CreateFromCheckpoint(ctx context.Context, spec orchestrator.Spec, srcVMID, checkpointID string) (orchestrator.Environment, error) {
	if p.stub != nil {
		return p.stub.CreateFromCheckpoint(ctx, spec, srcVMID, checkpointID)
	}

	reqBody := forkVMRequest{
		Name:       spec.Name,
		SnapshotID: checkpointID,
		CPUs:       spec.CPUs,
		MemoryMB:   spec.RamMB,
		StorageGB:  spec.StorageGB,
		Region:     spec.Region,
	}
	var resp createVMResponse
	path := fmt.Sprintf("/v1/vm/%s/fork", srcVMID)
	if err := p.doJSON(ctx, http.MethodPost, path, reqBody, &resp); err != nil {
		return nil, fmt.Errorf("firecracker fork vm %s from snapshot %s: %w", srcVMID, checkpointID, err)
	}

	env := &remoteEnv{
		id:     resp.VMID,
		url:    resp.URL,
		client: p,
	}
	if env.url == "" {
		env.url = fmt.Sprintf("fc://%s", resp.VMID)
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
		return nil, fmt.Errorf("firecracker get vm: %w", err)
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
		return nil, fmt.Errorf("firecracker list vms: %w", err)
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
		return orchestrator.HostCapacity{}, errors.New("firecracker: capacity probing requires a real host agent, not the in-memory stub")
	}
	var resp capacityResponse
	if err := p.doJSON(ctx, http.MethodGet, "/v1/capacity", nil, &resp); err != nil {
		return orchestrator.HostCapacity{}, fmt.Errorf("firecracker capacity: %w", err)
	}
	return orchestrator.HostCapacity{
		CPUs:       resp.CPUs,
		RamMB:      resp.RamMB,
		StorageGB:  resp.StorageGB,
		Arch:       resp.Arch,
		GPUs:       resp.GPUs,
		GPUKind:    resp.GPUKind,
		GPUDevices: resp.GPUDevices,
	}, nil
}

var _ orchestrator.CapacityProber = (*Provider)(nil)

// remoteEnv represents a Firecracker VM managed by the host agent.
type remoteEnv struct {
	id     string
	url    string
	client *Provider

	// authToken is the per-VM bearer token callers must present when
	// reaching url. It is populated by Boot via SetToken once
	// VMCredentials have been generated (and refreshed by token
	// rotation). Empty until then.
	tokenMu   sync.RWMutex
	authToken string

	// endpoints is populated by StartAgent from the host agent's response
	// when it publishes ingress ports. Nil when no ports were requested, or
	// when the host agent only supports the FROZEN /start-surfd wire (which
	// carries no endpoint data).
	endpointsMu sync.RWMutex
	endpoints   []orchestrator.Endpoint
}

var (
	_ orchestrator.Environment      = (*remoteEnv)(nil)
	_ orchestrator.Attacher         = (*remoteEnv)(nil)
	_ orchestrator.TokenSetter      = (*remoteEnv)(nil)
	_ orchestrator.EndpointReporter = (*remoteEnv)(nil)
	// firecracker is the only backend that can checkpoint guest memory; the
	// orchestrator finds that out by asserting on this interface, so the
	// assertion is pinned here rather than left to a caller to discover.
	_ orchestrator.LiveSnapshotCapable = (*remoteEnv)(nil)
)

// Endpoints returns the ingress endpoints published by the last StartAgent
// call, satisfying orchestrator.EndpointReporter.
func (e *remoteEnv) Endpoints() []orchestrator.Endpoint {
	e.endpointsMu.RLock()
	defer e.endpointsMu.RUnlock()
	return e.endpoints
}

func (e *remoteEnv) Name() string { return e.id }
func (e *remoteEnv) URL() string  { return e.url }

// Token returns the per-VM auth token (VMCredentials.AuthToken)
// previously installed via SetToken. Empty until Boot/rotation sets it.
func (e *remoteEnv) Token() string {
	e.tokenMu.RLock()
	defer e.tokenMu.RUnlock()
	return e.authToken
}

// SetToken records the per-VM auth token so subsequent Token() calls
// return it. Called by orchestrator.Boot and RotateToken via the
// orchestrator.TokenSetter interface.
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

// StartAgent launches the guest agent. It FIRST tries the generalized
// /start-agent endpoint (which additionally supports DownloadURL so OSS users
// can fetch the agent binary and skip a rootfs re-bake). Older host agents
// (e.g. the live host) only expose /start-surfd and return 404 for unknown
// actions; in that case we transparently FALL BACK to the FROZEN /start-surfd
// wire. Any non-404 error propagates.
//
// The structured fused-profile guest paths come from the local frozen-wire
// constants (the host agent owns the launch mechanism, not spec.Command), and
// only the generic spec fields (auth token, gateway) pass through.
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
	err := e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/start-agent", e.id), agentReq, &resp)
	if err == nil {
		e.setEndpoints(resp.Endpoints)
		return nil
	}
	var statusErr *orchestrator.HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusNotFound {
		return err
	}

	// Fall back to the FROZEN /start-surfd wire (unchanged payload, no
	// endpoint data — older host agents that only expose this wire don't
	// support ingress).
	req := startSurfdRequest{
		ManifestPath: fusedManifestGuestPath,
		SecretsPath:  fusedSecretsGuestPath,
		TLSCertPath:  fusedTLSCertGuestPath,
		TLSKeyPath:   fusedTLSKeyGuestPath,
		AuthToken:    spec.AuthToken,
		Gateway:      spec.Gateway,
		GatewayToken: spec.GatewayToken,
	}
	return e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/start-surfd", e.id), req, nil)
}

// setEndpoints records the endpoints reported by the last StartAgent call.
func (e *remoteEnv) setEndpoints(in []endpointWire) {
	out := make([]orchestrator.Endpoint, len(in))
	for i, ep := range in {
		out[i] = orchestrator.Endpoint{As: ep.As, URL: ep.URL, Port: ep.Port}
	}
	e.endpointsMu.Lock()
	e.endpoints = out
	e.endpointsMu.Unlock()
}

// toWireExpose converts orchestrator expose entries into the host-agent wire shape.
func toWireExpose(in []orchestrator.ExposeSpec) []exposeWire {
	if len(in) == 0 {
		return nil
	}
	out := make([]exposeWire, len(in))
	for i, ex := range in {
		out[i] = exposeWire{Port: ex.Port, As: ex.As}
	}
	return out
}

func (e *remoteEnv) Checkpoint(ctx context.Context, comment string) (string, error) {
	cp, err := e.CheckpointWithDigest(ctx, comment)
	return cp.ID, err
}

// CheckpointWithDigest implements orchestrator.SnapshotDigester. The agent
// hashes the artifact inline as it writes it, so this response is the only
// place the digest is ever available; nothing recomputes it later.
func (e *remoteEnv) CheckpointWithDigest(ctx context.Context, comment string) (orchestrator.Checkpoint, error) {
	return e.snapshot(ctx, comment, false)
}

// CheckpointLive implements orchestrator.LiveSnapshotCapable: rootfs plus vCPU
// state plus the whole guest memory, taken inside one pause window on the
// agent side so the memory image and the disk it describes cannot drift apart.
//
// Only the flag differs from the disk path; it is the same verb against the
// same store and it yields the same kind of id. That is deliberate on the
// agent's side too, and it means an agent too old to know about `live` does
// not fail, it takes a disk snapshot and says so in the response kind. The
// caller sees what it actually got rather than a success that quietly means
// something else.
func (e *remoteEnv) CheckpointLive(ctx context.Context, comment string) (orchestrator.Checkpoint, error) {
	return e.snapshot(ctx, comment, true)
}

// snapshot is the one wire call behind both checkpoint paths.
func (e *remoteEnv) snapshot(ctx context.Context, comment string, live bool) (orchestrator.Checkpoint, error) {
	req := snapshotRequest{Comment: comment, Live: live}
	var resp snapshotResponse
	if err := e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/snapshot", e.id), req, &resp); err != nil {
		return orchestrator.Checkpoint{}, fmt.Errorf("snapshot: %w", err)
	}
	// The kind comes back from the agent rather than being assumed from what
	// was asked for: the agent is the only thing that knows what it managed to
	// write, and an agent build predating live snapshots answers with no kind
	// at all, which normalizes to disk.
	return orchestrator.Checkpoint{
		ID:        resp.SnapshotID,
		Comment:   comment,
		Digest:    resp.Digest,
		SizeBytes: resp.SizeBytes,
		Kind:      snapshotKind(resp.Kind),
	}, nil
}

// snapshotKind normalizes the agent's wire value. Anything unrecognised or
// absent is disk, which is the safe direction: treating a disk snapshot as
// live would have a restore ask firecracker for a memory file that does not
// exist, while the reverse only costs a cold boot.
func snapshotKind(s string) orchestrator.SnapshotKind {
	if s == string(orchestrator.SnapshotKindLive) {
		return orchestrator.SnapshotKindLive
	}
	return orchestrator.SnapshotKindDisk
}

func (e *remoteEnv) Restore(ctx context.Context, checkpointID string) error {
	req := restoreRequest{SnapshotID: checkpointID}
	return e.client.doJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/vm/%s/restore", e.id), req, nil)
}

func (e *remoteEnv) DeleteCheckpoint(ctx context.Context, checkpointID string) error {
	return e.client.doJSON(ctx, http.MethodDelete, fmt.Sprintf("/v1/vm/%s/snapshots/%s", e.id, checkpointID), nil, nil)
}

func (e *remoteEnv) ListCheckpoints(ctx context.Context) ([]orchestrator.Checkpoint, error) {
	var resp listSnapshotsResponse
	if err := e.client.doJSON(ctx, http.MethodGet, fmt.Sprintf("/v1/vm/%s/snapshots", e.id), nil, &resp); err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	out := make([]orchestrator.Checkpoint, 0, len(resp.Snapshots))
	for _, s := range resp.Snapshots {
		out = append(out, orchestrator.Checkpoint{
			ID:        s.SnapshotID,
			Comment:   s.Comment,
			SizeBytes: s.SizeBytes,
			CreatedAt: s.CreatedAt,
			Kind:      snapshotKind(s.Kind),
		})
	}
	return out, nil
}

// HTTP helpers and request/response shapes.

type createVMRequest struct {
	Name      string `json:"name"`
	CPUs      int    `json:"cpus"`
	MemoryMB  int    `json:"memory_mb"`
	StorageGB int    `json:"storage_gb"`
	Region    string `json:"region"`
	Image     string `json:"image,omitempty"`
	// SeedSnapshot boots from an artifact in the agent's snapshot store rather
	// than a named base image. The agent treats it as winning over Image, but
	// the orchestrator rejects a request carrying both before it gets here.
	SeedSnapshot string `json:"seed_snapshot,omitempty"`
}

type createVMResponse struct {
	VMID string `json:"vm_id"`
	URL  string `json:"url"`
}

// forkVMRequest seeds a new vm from an existing vm's snapshot. The host agent
// resolves snapshot_id against the source vm in the URL path and copies that
// snapshot's rootfs into the new vm, so no image is carried here. Sizing
// fields left zero default to the source vm's on the host side.
type forkVMRequest struct {
	Name       string `json:"name"`
	SnapshotID string `json:"snapshot_id"`
	CPUs       int    `json:"cpus,omitempty"`
	MemoryMB   int    `json:"memory_mb,omitempty"`
	StorageGB  int    `json:"storage_gb,omitempty"`
	Region     string `json:"region,omitempty"`
}

type getVMResponse struct {
	VMID string `json:"vm_id"`
	URL  string `json:"url"`
}

// capacityResponse is the GET /v1/capacity response: the host agent's real
// CPU count, total RAM, and free disk. Firecracker hosts have no GPU
// passthrough, so the GPU fields are always zero/empty; they are present for
// parity with the qemu wire shape.
type capacityResponse struct {
	CPUs       int                      `json:"cpus"`
	RamMB      int                      `json:"ram_mb"`
	StorageGB  int                      `json:"storage_gb"`
	Arch       string                   `json:"arch"`
	GPUs       int                      `json:"gpus"`
	GPUKind    string                   `json:"gpu_kind"`
	GPUDevices []orchestrator.GPUDevice `json:"gpu_devices"`
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

type startSurfdRequest struct {
	ManifestPath string `json:"manifest_path"`
	SecretsPath  string `json:"secrets_path"`
	TLSCertPath  string `json:"tls_cert_path,omitempty"`
	TLSKeyPath   string `json:"tls_key_path,omitempty"`
	AuthToken    string `json:"auth_token,omitempty"`
	Gateway      string `json:"gateway,omitempty"`
	GatewayToken string `json:"gateway_token,omitempty"`
}

// startAgentRequest is the generalized launch wire. It carries the same fields
// as startSurfdRequest plus optional download_url/binary_path/listen so OSS
// users can fetch the agent binary and/or run a custom in-guest daemon.
type startAgentRequest struct {
	ManifestPath string       `json:"manifest_path"`
	SecretsPath  string       `json:"secrets_path"`
	TLSCertPath  string       `json:"tls_cert_path,omitempty"`
	TLSKeyPath   string       `json:"tls_key_path,omitempty"`
	AuthToken    string       `json:"auth_token,omitempty"`
	Gateway      string       `json:"gateway,omitempty"`
	GatewayToken string       `json:"gateway_token,omitempty"`
	DownloadURL  string       `json:"download_url,omitempty"`
	BinaryPath   string       `json:"binary_path,omitempty"`
	Listen       string       `json:"listen,omitempty"`
	Expose       []exposeWire `json:"expose,omitempty"`
}

// exposeWire/endpointWire are the host-agent wire shapes for ingress,
// carried only on the generalized /start-agent request (never on the
// FROZEN /start-surfd wire).
type exposeWire struct {
	Port int    `json:"port"`
	As   string `json:"as,omitempty"`
}

type endpointWire struct {
	As   string `json:"as,omitempty"`
	URL  string `json:"url"`
	Port int    `json:"port"`
}

// startAgentResponse is the /start-agent response body. Endpoints is
// populated only when the request declared Expose entries and the host
// agent published them.
type startAgentResponse struct {
	Endpoints []endpointWire `json:"endpoints,omitempty"`
}

type snapshotRequest struct {
	Comment string `json:"comment,omitempty"`

	// Live asks the agent for a memory snapshot alongside the rootfs. It is
	// omitempty because it is opt-in on both ends: an omitted flag is a disk
	// snapshot, which is what every caller predating this got and what an
	// agent that does not read the field will do anyway.
	Live bool `json:"live,omitempty"`
}

type snapshotResponse struct {
	SnapshotID string `json:"snapshot_id"`

	// Digest is the hex sha256 the agent computed over the artifact rootfs it
	// just wrote. It is absent from an agent build that predates artifact
	// hashing, which decodes as "" and must stay a non-error: an operator
	// running an older agent still gets working snapshots, just no integrity
	// value to verify a later transfer against.
	Digest string `json:"digest,omitempty"`

	// Kind is what the agent actually wrote, "disk" or "live". Absent from an
	// agent predating live snapshots, which decodes as "" and normalizes to
	// disk (see snapshotKind).
	Kind string `json:"kind,omitempty"`

	// SizeBytes is what the snapshot occupies on the host, reported at create
	// time because a live snapshot is a different order of size from a disk
	// one (a full memory image every time) and nothing upstream can stat it.
	SizeBytes int64 `json:"size_bytes,omitempty"`
}

// restoreRequest carries only the id. Whether a restore resumes from memory or
// cold boots is not the caller's to choose: the agent recorded the kind when it
// wrote the snapshot and branches on that, so a caller cannot ask for a memory
// restore of an artifact that has no memory in it.
type restoreRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

type listSnapshotsResponse struct {
	Snapshots []struct {
		SnapshotID string    `json:"snapshot_id"`
		Comment    string    `json:"comment,omitempty"`
		SizeBytes  int64     `json:"size_bytes,omitempty"`
		CreatedAt  time.Time `json:"created_at,omitempty"`
		Kind       string    `json:"kind,omitempty"`
	} `json:"snapshots"`
}

// TODO: Add retry logic with exponential backoff for transient failures
// (HTTP 500/502/503, connection reset). Idempotent operations (GET, DELETE)
// are safe to retry immediately; POST /v1/vm should only retry if the
// response was never received (connection error, not a 500 with a body).
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
	dec := json.NewDecoder(res.Body)
	return dec.Decode(respBody)
}

// stubProvider is the in-memory fallback used when BaseURL is empty or UseStub is set.
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
		name:     spec.Name,
		url:      fmt.Sprintf("fc://%s", spec.Name),
		image:    spec.Image,
		seed:     spec.SeedSnapshotID,
		memoryMB: spec.RamMB,
	}
	p.envs[spec.Name] = env
	return env, nil
}

// CreateFromCheckpoint seeds a new stub env from an existing one's checkpoint,
// mirroring the remote path's semantics: the source must exist and must own the
// checkpoint, and the new env is keyed by spec.Name. The seed checkpoint and the
// source's files are copied in so the fork "boots" the snapshot's disk state,
// which is what makes the stub useful for exercising fork end to end.
func (p *stubProvider) CreateFromCheckpoint(_ context.Context, spec orchestrator.Spec, srcVMID, checkpointID string) (orchestrator.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	src, ok := p.envs[srcVMID]
	if !ok {
		return nil, fmt.Errorf("source env %s not found", srcVMID)
	}
	if _, exists := p.envs[spec.Name]; exists {
		return nil, fmt.Errorf("env %s already exists", spec.Name)
	}

	src.mu.Lock()
	var (
		seed  orchestrator.Checkpoint
		found bool
	)
	for _, cp := range src.checkpoints {
		if cp.ID == checkpointID {
			seed, found = cp, true
			break
		}
	}
	files := make(map[string][]byte, len(src.files))
	for path, data := range src.files {
		cp := make([]byte, len(data))
		copy(cp, data)
		files[path] = cp
	}
	image := src.image
	src.mu.Unlock()

	if !found {
		return nil, fmt.Errorf("checkpoint %s not found on %s", checkpointID, srcVMID)
	}

	env := &stubEnv{
		name:        spec.Name,
		url:         fmt.Sprintf("fc://%s", spec.Name),
		image:       image,
		files:       files,
		checkpoints: []orchestrator.Checkpoint{seed},
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
		if prefix == "" || (len(name) >= len(prefix) && strings.HasPrefix(name, prefix)) {
			envs = append(envs, env)
		}
	}
	return envs, nil
}

func (p *stubProvider) Close() error { return nil }

// stubEnv simulates a VM in memory.
type stubEnv struct {
	name  string
	url   string
	image string // spec.Image at Create time, kept for test inspection only
	seed  string // spec.SeedSnapshotID at Create time, same purpose as image
	// memoryMB is spec.RamMB at Create time, kept only so a live checkpoint
	// can report a size that behaves like the real one: memory dominates it,
	// and code that sizes or quotas live snapshots should not be exercised
	// against a stub where the two kinds are indistinguishable.
	memoryMB int

	mu          sync.Mutex
	files       map[string][]byte
	checkpoints []orchestrator.Checkpoint
	authToken   string
	endpoints   []orchestrator.Endpoint
}

var (
	_ orchestrator.Environment         = (*stubEnv)(nil)
	_ orchestrator.TokenSetter         = (*stubEnv)(nil)
	_ orchestrator.EndpointReporter    = (*stubEnv)(nil)
	_ orchestrator.LiveSnapshotCapable = (*stubEnv)(nil)
)

// Endpoints returns endpoints synthesized by the last StartAgent call,
// satisfying orchestrator.EndpointReporter. The stub has no real guest or
// host firewall, so it fabricates a plausible fc://<name>:<port> URL per
// requested expose entry — enough to prove the wiring flows end to end in
// hermetic tests without claiming the port is actually reachable.
func (e *stubEnv) Endpoints() []orchestrator.Endpoint {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.endpoints
}

func (e *stubEnv) Name() string { return e.name }
func (e *stubEnv) URL() string  { return e.url }

// Token returns the per-VM auth token previously installed via SetToken.
func (e *stubEnv) Token() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.authToken
}

// SetToken records the per-VM auth token so subsequent Token() calls
// return it.
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

// ExecStream simulates a successful guest command (no output written). Like
// Exec, the stub succeeds so the dev server can exercise drain and other
// guest-exec paths without a real microVM.
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
	if len(spec.Expose) == 0 {
		return nil
	}
	endpoints := make([]orchestrator.Endpoint, len(spec.Expose))
	for i, ex := range spec.Expose {
		endpoints[i] = orchestrator.Endpoint{
			As:   ex.As,
			URL:  fmt.Sprintf("fc://%s:%d", e.name, ex.Port),
			Port: ex.Port,
		}
	}
	e.mu.Lock()
	e.endpoints = endpoints
	e.mu.Unlock()
	return nil
}

func (e *stubEnv) Checkpoint(_ context.Context, comment string) (string, error) {
	cp := e.appendCheckpoint(comment, orchestrator.SnapshotKindDisk)
	return cp.ID, nil
}

// CheckpointLive implements orchestrator.LiveSnapshotCapable. The stub has no
// guest memory to write, but it reports the kind and a memory-inclusive size
// so anything that branches on either is exercised the same way it would be
// against a real agent.
func (e *stubEnv) CheckpointLive(_ context.Context, comment string) (orchestrator.Checkpoint, error) {
	return e.appendCheckpoint(comment, orchestrator.SnapshotKindLive), nil
}

func (e *stubEnv) appendCheckpoint(comment string, kind orchestrator.SnapshotKind) orchestrator.Checkpoint {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := fmt.Sprintf("cp-%d", len(e.checkpoints)+1)
	var sizeBytes int64
	for _, file := range e.files {
		sizeBytes += int64(len(file))
	}
	if kind == orchestrator.SnapshotKindLive {
		// the whole memory image lands on disk every time, with no sparseness
		// to hide behind, which is the property that makes live snapshots
		// expensive and is worth reproducing here.
		sizeBytes += int64(e.memoryMB) * 1024 * 1024
	}
	cp := orchestrator.Checkpoint{
		ID:        id,
		Comment:   comment,
		SizeBytes: sizeBytes,
		CreatedAt: time.Now(),
		Kind:      kind,
	}
	e.checkpoints = append(e.checkpoints, cp)
	return cp
}

func (e *stubEnv) Restore(_ context.Context, checkpointID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, cp := range e.checkpoints {
		if cp.ID == checkpointID {
			return nil
		}
	}
	return fmt.Errorf("checkpoint %s not found", checkpointID)
}

func (e *stubEnv) DeleteCheckpoint(_ context.Context, checkpointID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, cp := range e.checkpoints {
		if cp.ID != checkpointID {
			continue
		}
		e.checkpoints = append(e.checkpoints[:i], e.checkpoints[i+1:]...)
		return nil
	}
	return fmt.Errorf("checkpoint %s not found", checkpointID)
}

func (e *stubEnv) ListCheckpoints(_ context.Context) ([]orchestrator.Checkpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cps := make([]orchestrator.Checkpoint, len(e.checkpoints))
	copy(cps, e.checkpoints)
	return cps, nil
}
