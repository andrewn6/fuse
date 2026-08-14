package fusefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ResourceSpec mirrors internal/api.ResourceSpec field-for-field so the CLI can
// copy Compiled.Spec into the sdk/api spec without importing the api package.
//
// Image selects the VM's base rootfs at create time (a name resolved by the
// firecracker host agent to a pre-baked rootfs file; see FUSEFILE_PLAN.md
// Phase 7). It lives here, not in the manifest json, because rootfs selection
// happens at Provider.Create — before the guest boots and long before the
// manifest is ever uploaded to it.
type ResourceSpec struct {
	CPUs      int32
	RamMB     int32
	StorageGB int32
	Region    string
	// Arch restricts scheduling to hosts of this CPU architecture ("amd64",
	// "arm64"). Not yet authorable in a Fusefile; carried for wire parity
	// so compile output mirrors the orchestrator's spec shape.
	Arch              string
	MaxRuntimeSeconds int64
	// IdleTimeoutSeconds destroys the environment after this many seconds
	// with no exec and no attach session. Zero means no idle expiry.
	IdleTimeoutSeconds int64
	Image              string
	GPUs               int32
	GPUKind            string
	GPUProfile         string
	// HostID pins the environment to an exact host id (placement.host).
	HostID string
	// Labels are the placement label selectors (placement.labels); every
	// pair must match the host's declared labels.
	Labels map[string]string
}

// ExposeSpec requests that a guest port be published as a reachable
// endpoint. Mirrors Fusefile's Expose entries one-for-one.
type ExposeSpec struct {
	Port int
	As   string
}

// Compiled is the result of compiling a Fusefile: the resource spec, the
// manifest json to upload to the guest, the startup script to run, the
// secrets the environment needs at create time, and any ports to expose.
type Compiled struct {
	Spec            ResourceSpec
	ManifestJSON    []byte
	StartupScript   string
	RequiredSecrets []string
	Expose          []ExposeSpec

	// BuildScript is the setup phase alone, for `fuse build` to run through
	// the exec path (600s) rather than the startup-script path (30s).
	// StartupScript still carries setup+run, so `fuse up` is unchanged.
	BuildScript string

	// RunScript is the run command alone, for a boot whose setup phase is
	// already baked into a seed rootfs (`fuse up --from-build`). Replaying
	// setup there would redo the work the artifact exists to skip.
	RunScript string

	// StartupTimeoutSeconds carries Fusefile.StartupTimeout to the wire.
	// Zero means the author did not ask for one and the orchestrator's
	// default applies.
	StartupTimeoutSeconds int64
}

// MaxFilesBytes caps the total decoded size of Fusefile.Files.
//
// The binding constraint is not the 1 MiB create body (api.MaxEnvironmentBodyBytes)
// but the way the script is delivered: the orchestrator runs it as
// `sh -lc <script>` and the host agent joins that into one shell word before
// handing it to ssh, so the whole script ends up as a single argv element.
// Linux caps one argument at MAX_ARG_STRLEN, 128 KiB, and overflowing it fails
// the boot with a bare E2BIG rather than anything an author could act on.
//
// 64 KiB of files becomes roughly 88 KiB of base64 once wrapped, which leaves
// around 40 KiB for the prelude, setup, and run. Exceeding the cap is a compile
// error rather than a runtime failure, so the author learns at `fuse up` time
// which file pushed them over.
const MaxFilesBytes = 64 << 10

// defaultWorkspace is the directory setup and run execute in when
// Fusefile.Workspace is unset.
const defaultWorkspace = "/workspace"

// workspaceOf returns the effective workspace: the authored value, or
// defaultWorkspace when it is unset. Every consumer of the workspace goes
// through this (the manifest, the script prelude, the layer key), so the
// default cannot drift between them.
func workspaceOf(f *Fusefile) string {
	if f.Workspace == "" {
		return defaultWorkspace
	}
	return f.Workspace
}

// MinIdleTimeout is the smallest meaningful resources.idle_timeout. Idle
// expiry is detected on the orchestrator's reconcile loop (30s default) and
// requires two consecutive observations, so anything shorter than this
// promises a precision the loop cannot deliver.
const MinIdleTimeout = time.Minute

// manifest is the compiler-local marshal type for the guest-facing manifest
// json. it mirrors DefaultFusedManifest (internal/orchestrator/agent_profile.go)
// and the shape internal/secrets.ExtractRequiredSecrets reads; there is no
// shared Go struct for it since this package must not import either.
type manifest struct {
	Version  string                     `json:"version"`
	Machine  manifestMachine            `json:"machine"`
	Services map[string]manifestService `json:"services"`
}

// manifestMachine carries the machine block of the guest-facing manifest.
//
// Workspace here is RESERVED, not a knob: nothing in the stack reads it. The
// guest agent (cmd/fused) reports the manifest's size and never parses the
// machine block, and the host agents do not look at it either. What actually
// puts an environment in its workspace is the generated script's `mkdir -p`
// plus `cd` (see scriptPrelude), which is emitted from the same value.
//
// It is still emitted, and still defaulted, so the wire shape matches
// DefaultFusedManifest (internal/orchestrator/agent_profile.go) and so a guest
// agent that wants to honor it later has the value already on hand. Giving it a
// consumer is a guest-agent change, not a compiler one.
type manifestMachine struct {
	Workspace string `json:"workspace"`
}

type manifestService struct {
	Image       string                 `json:"image,omitempty"`
	Ports       []int                  `json:"ports,omitempty"`
	Env         map[string]manifestEnv `json:"env,omitempty"`
	Command     []string               `json:"command,omitempty"`
	Restart     string                 `json:"restart,omitempty"`
	HealthCheck *manifestHealthCheck   `json:"healthcheck,omitempty"`
	DependsOn   []string               `json:"depends_on,omitempty"`
}

type manifestHealthCheck struct {
	Test     []string `json:"test,omitempty"`
	Interval string   `json:"interval,omitempty"`
	Timeout  string   `json:"timeout,omitempty"`
	Retries  int      `json:"retries,omitempty"`
}

type manifestEnv struct {
	Value  string `json:"value,omitempty"`
	Secret string `json:"secret,omitempty"`
}

// sizePattern matches a number, optional whitespace, and a unit: "512MB",
// "2G", "2 GB", "1.5GiB", "2TB". matching is done against an uppercased copy
// of the input so the unit is effectively case-insensitive. a bare number has
// no unit and stays rejected: it is genuinely ambiguous.
//
// every unit is binary, so GB and GiB are synonyms (1024 MiB) and always have
// been: parseSize has multiplied GB by 1024 since v1, and RamMB becomes
// firecracker's mem_size_mib and qemu's -m. making GB decimal would silently
// shrink every existing `memory: 2GB` by 7 percent, so it is not done here.
var sizePattern = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*(M|MB|MIB|G|GB|GIB|T|TB|TIB)$`)

// sizeUnitsMiB maps each accepted unit to its size in MiB.
var sizeUnitsMiB = map[string]int64{
	"M": 1, "MB": 1, "MIB": 1,
	"G": 1024, "GB": 1024, "GIB": 1024,
	"T": 1024 * 1024, "TB": 1024 * 1024, "TIB": 1024 * 1024,
}

// gpuProfilePattern matches nvidia mig-parted profile names: <slices>g.<mem>gb
// with an optional "+me" media-extensions suffix (e.g. "1g.10gb", "3g.20gb",
// "1g.10gb+me"). MIG supports at most 7 slices per GPU. matching is done
// against a lowercased copy so the profile is effectively case-insensitive.
var gpuProfilePattern = regexp.MustCompile(`^[1-7]g\.\d+gb(\+me)?$`)

// ValidGPUProfile reports whether s is a well-formed MIG profile name
// ("1g.10gb", "2g.20gb", ...). Shared with the API layer's request
// validation so raw SDK callers are held to the same vocabulary as
// Fusefile authors.
func ValidGPUProfile(s string) bool {
	return gpuProfilePattern.MatchString(strings.ToLower(s))
}

// labelPattern is the accepted form for a placement label key or value:
// alphanumeric at both ends, dots, dashes, and underscores inside, 63 chars
// max. Deliberately narrow so a label is safe to render in an error message
// and to compare byte-for-byte against a host's declared labels.
var labelPattern = regexp.MustCompile(`^[a-zA-Z0-9]([-._a-zA-Z0-9]{0,61}[a-zA-Z0-9])?$`)

// ValidLabel reports whether s is a well-formed placement label key or value.
// Shared with the API layer so raw SDK callers and host registration are held
// to the same vocabulary as Fusefile authors.
func ValidLabel(s string) bool {
	return labelPattern.MatchString(s)
}

// exposeNamePattern is the accepted form for expose[].as: a DNS label.
// lowercase letters, digits and dashes, alphanumeric at both ends, 63 chars
// max. The name is how an endpoint is addressed, so it is held to what a
// hostname component can carry rather than to arbitrary text.
var exposeNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ValidExposeName reports whether s is a well-formed expose[].as name.
func ValidExposeName(s string) bool {
	return exposeNamePattern.MatchString(s)
}

// validRestartPolicies is the compose-native restart policy vocabulary.
// Compose also accepts "on-failure:N" for a bounded retry count, but that
// extension is deliberately not accepted here to keep the validated set
// exact and unambiguous.
var validRestartPolicies = map[string]bool{
	"no": true, "always": true, "on-failure": true, "unless-stopped": true,
}

// nonMIGCapableKinds are GPU model families that are known NOT to support
// MIG. Requesting a gpu_profile alongside one of these is a definite mistake,
// so it is rejected at request time. The list is deliberately a denylist of
// well-known consumer/older parts rather than an allowlist of MIG-capable
// datacenter parts: an unrecognized kind passes here and is enforced at
// scheduling time against the host's per-device MIGCapable flag (see
// orchestrator.hostServesMIG), so a valid future GPU is never blocked by a
// stale allowlist. That check only fires for hosts that report per-device
// inventory; a host that reports none is left to the qemu agent.
var nonMIGCapableKinds = map[string]bool{
	"v100": true, "t4": true, "p100": true, "p40": true, "k80": true,
	"rtx": true, "gtx": true, "titan": true, "l4": true, "l40": true, "l40s": true,
}

// KindSupportsMIG reports whether a GPU kind can plausibly run a MIG profile.
// An empty kind (unknown at request time) and any unrecognized kind return
// true so the scheduler's per-device MIGCapable flag remains the source of
// truth; only a kind on the known non-MIG denylist returns false. Matching is
// case-insensitive and substring-based so "NVIDIA V100" and "v100-sxm2" both
// resolve.
func KindSupportsMIG(kind string) bool {
	if kind == "" {
		return true
	}
	k := strings.ToLower(kind)
	for bad := range nonMIGCapableKinds {
		if strings.Contains(k, bad) {
			return false
		}
	}
	return true
}

// Compile turns the human-friendly Fusefile.Resources into a ResourceSpec.
func Compile(f *Fusefile) (*Compiled, error) {
	var errs []error

	ramMB, err := parseSize(f.Resources.Memory)
	if err != nil {
		errs = append(errs, fmt.Errorf("resources.memory: %w", err))
	}

	storageMB, err := parseSize(f.Resources.Storage)
	if err != nil {
		errs = append(errs, fmt.Errorf("resources.storage: %w", err))
	}

	diskMB, err := parseSize(f.Resources.Disk)
	if err != nil {
		errs = append(errs, fmt.Errorf("resources.disk: %w", err))
	}

	// disk is the preferred spelling and storage is a permanent alias. there
	// is only one root disk to size, so setting both to different sizes is an
	// author mistake rather than something to silently resolve.
	switch {
	case f.Resources.Disk != "" && f.Resources.Storage != "":
		if diskMB != storageMB {
			errs = append(errs, fmt.Errorf(
				"resources.disk and resources.storage are the same field, but were set to %q and %q",
				f.Resources.Disk, f.Resources.Storage))
		}
		storageMB = diskMB
	case f.Resources.Disk != "":
		storageMB = diskMB
	}

	var maxRuntimeSeconds int64
	if f.Resources.MaxRuntime != "" {
		d, err := time.ParseDuration(f.Resources.MaxRuntime)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("resources.max_runtime: %w", err))
		case d < 0:
			errs = append(errs, fmt.Errorf("resources.max_runtime: must not be negative"))
		default:
			maxRuntimeSeconds = int64(d.Seconds())
		}
	}

	var idleTimeoutSeconds int64
	if f.Resources.IdleTimeout != "" {
		d, err := time.ParseDuration(f.Resources.IdleTimeout)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("resources.idle_timeout: %w", err))
		case d < 0:
			errs = append(errs, fmt.Errorf("resources.idle_timeout: must not be negative"))
		case d > 0 && d < MinIdleTimeout:
			// idle expiry is only as accurate as the reconcile tick plus the
			// two-strike rule, so sub-minute windows would be misleading.
			errs = append(errs, fmt.Errorf(
				"resources.idle_timeout: must be at least %s (idle detection runs on the reconcile loop)",
				MinIdleTimeout))
		default:
			idleTimeoutSeconds = int64(d.Seconds())
		}
	}

	var startupTimeoutSeconds int64
	if f.StartupTimeout != "" {
		d, err := time.ParseDuration(f.StartupTimeout)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("startup_timeout: %w", err))
		case d <= 0:
			errs = append(errs, fmt.Errorf("startup_timeout: must be positive, got %q", f.StartupTimeout))
		default:
			// Round up so a sub-second request is never floored to zero,
			// which the wire reads as "unset" and would silently hand back
			// the orchestrator default instead of the bound asked for.
			startupTimeoutSeconds = int64((d + time.Second - 1) / time.Second)
		}
	}

	// zero keeps its meaning: the field was omitted and the host agent picks
	// its default vCPU count. only a negative count is rejected.
	if f.Resources.CPUs < 0 {
		errs = append(errs, fmt.Errorf("resources.cpus: must not be negative"))
	}

	if f.Resources.GPU < 0 {
		errs = append(errs, fmt.Errorf("resources.gpu: must not be negative"))
	}

	errs = append(errs, validateFiles(f.Files)...)

	if f.Resources.GPUProfile != "" {
		if !ValidGPUProfile(f.Resources.GPUProfile) {
			errs = append(errs, fmt.Errorf(
				"resources.gpu_profile: invalid MIG profile %q (expected mig-parted form like \"1g.10gb\")",
				f.Resources.GPUProfile))
		}
		if f.Resources.GPU == 0 {
			errs = append(errs, fmt.Errorf(
				"resources.gpu_profile: requires resources.gpu >= 1 (the count of MIG instances)"))
		}
		if !KindSupportsMIG(f.Resources.GPUKind) {
			errs = append(errs, fmt.Errorf(
				"resources.gpu_profile: %q does not support MIG (gpu_kind %q)",
				f.Resources.GPUProfile, f.Resources.GPUKind))
		}
	}

	// sorted for a deterministic error order when multiple services are invalid.
	serviceNames := make([]string, 0, len(f.Services))
	for name := range f.Services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	for _, name := range serviceNames {
		if restart := f.Services[name].Restart; restart != "" && !validRestartPolicies[restart] {
			errs = append(errs, fmt.Errorf(
				"services.%s.restart: invalid %q (want one of: no, always, on-failure, unless-stopped)",
				name, restart))
		}
	}

	manifestJSON, requiredSecrets, err := compileManifest(f)
	if err != nil {
		errs = append(errs, err)
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	return &Compiled{
		Spec: ResourceSpec{
			CPUs:  int32(f.Resources.CPUs),
			RamMB: ramMB,
			// round up so any positive storage request is never silently zeroed
			// (e.g. "512MB" must provision 1GB, not floor to 0).
			StorageGB:          int32((int64(storageMB) + 1023) / 1024),
			Region:             f.Resources.Region,
			MaxRuntimeSeconds:  maxRuntimeSeconds,
			IdleTimeoutSeconds: idleTimeoutSeconds,
			Image:              f.Image,
			GPUs:               int32(f.Resources.GPU),
			GPUKind:            f.Resources.GPUKind,
			GPUProfile:         strings.ToLower(f.Resources.GPUProfile),
			HostID:             f.Placement.Host,
			Labels:             compileLabels(f.Placement.Labels),
		},
		ManifestJSON:          manifestJSON,
		StartupScript:         compileStartupScript(f),
		BuildScript:           compileBuildScript(f),
		RunScript:             compileRunScript(f),
		RequiredSecrets:       requiredSecrets,
		Expose:                compileExpose(f),
		StartupTimeoutSeconds: startupTimeoutSeconds,
	}, nil
}

// compileLabels copies the placement label selectors so the compiled spec
// never aliases the parsed Fusefile. Nil (and empty) in, nil out.
func compileLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// compileExpose carries Fusefile.Expose through unchanged, one-for-one.
func compileExpose(f *Fusefile) []ExposeSpec {
	if len(f.Expose) == 0 {
		return nil
	}
	out := make([]ExposeSpec, len(f.Expose))
	for i, e := range f.Expose {
		out[i] = ExposeSpec(e)
	}
	return out
}

// compileManifest builds the guest-facing manifest json and the sorted,
// deduped union of secrets it references (plus f.Secrets).
func compileManifest(f *Fusefile) ([]byte, []string, error) {
	secretSet := make(map[string]bool, len(f.Secrets))
	for _, s := range f.Secrets {
		secretSet[s] = true
	}

	m := manifest{
		Version:  "1",
		Machine:  manifestMachine{Workspace: workspaceOf(f)},
		Services: make(map[string]manifestService, len(f.Services)),
	}

	for name, svc := range f.Services {
		ms := manifestService{Image: svc.Image, Restart: svc.Restart}
		if len(svc.Ports) > 0 {
			ms.Ports = append([]int(nil), svc.Ports...)
		}
		if len(svc.Command) > 0 {
			ms.Command = append([]string(nil), svc.Command...)
		}
		if len(svc.DependsOn) > 0 {
			ms.DependsOn = append([]string(nil), svc.DependsOn...)
		}
		if svc.HealthCheck != nil {
			ms.HealthCheck = &manifestHealthCheck{
				Test:     append([]string(nil), svc.HealthCheck.Test...),
				Interval: svc.HealthCheck.Interval,
				Timeout:  svc.HealthCheck.Timeout,
				Retries:  svc.HealthCheck.Retries,
			}
		}
		if len(svc.Env) > 0 {
			ms.Env = make(map[string]manifestEnv, len(svc.Env))
			for key, ev := range svc.Env {
				me := manifestEnv(ev)
				if ev.Secret != "" {
					secretSet[ev.Secret] = true
				}
				ms.Env[key] = me
			}
		}
		m.Services[name] = ms
	}

	manifestJSON, err := json.Marshal(m)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal manifest: %w", err)
	}

	var requiredSecrets []string
	for s := range secretSet {
		requiredSecrets = append(requiredSecrets, s)
	}
	sort.Strings(requiredSecrets)

	return manifestJSON, requiredSecrets, nil
}

// shellQuote renders s as a single POSIX shell word. The workspace path is
// author-supplied, so it is quoted rather than interpolated raw: a path with
// a space, a quote, or a metacharacter must not be able to alter the
// generated script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// IsEmpty reports whether the command is unset: neither the shell form nor the
// exec form was provided. The compiler treats an empty command the same as an
// omitted `run` (no line emitted, setup-only or build-only scripts unaffected).
func (c Command) IsEmpty() bool {
	return c.Shell == "" && len(c.Argv) == 0
}

// Render produces the shell line emitted for the command. The shell form is
// passed through byte for byte so a scalar `run` compiles to exactly the
// script it always did; the exec form is built from shellQuote'd elements so an
// argument value (spaces, quotes, glob characters) can never be reinterpreted
// by the shell.
func (c Command) Render() string {
	if len(c.Argv) > 0 {
		quoted := make([]string, len(c.Argv))
		for i, arg := range c.Argv {
			quoted[i] = shellQuote(arg)
		}
		return strings.Join(quoted, " ")
	}
	return c.Shell
}

// setupScripts returns the shell fragment emitted for each setup step, in
// order. compileStartupScript concatenates exactly this, and layer key
// derivation hashes exactly this, so the key and the emitted script cannot
// drift apart. a step's fragment is its `run` string byte for byte: no
// normalization, because guessing at semantic equivalence is how a cache
// serves a stale rootfs.
//
// a step with a `workdir` is the one exception: it is wrapped in a subshell,
// `(cd <quoted>; <run>)`, so the directory change is scoped to that step and
// the next one still starts in the workspace. a relative workdir resolves
// against the workspace because that is where the shell already is. only the
// directory is scoped: setup steps share one shell, so `set -eu`, exported
// variables, and shell options still carry across steps either way.
//
// the wrapper is part of the hashed fragment, so adding or changing a workdir
// invalidates that step's layer, as it should.
func setupScripts(f *Fusefile) []string {
	if len(f.Setup) == 0 {
		return nil
	}
	out := make([]string, len(f.Setup))
	for i, step := range f.Setup {
		if step.Workdir == "" {
			out[i] = step.Run
			continue
		}
		out[i] = fmt.Sprintf("(cd %s; %s)", shellQuote(step.Workdir), step.Run)
	}
	return out
}

// scriptPrelude renders the strict-mode header and the workspace cd that every
// generated script shares.
//
// posix prelude: the orchestrator runs these via `sh -lc`, and dash (ubuntu's
// /bin/sh) has no pipefail, so enable it only when supported.
//
// the workspace is where setup and run are meant to execute, so create it and
// move into it. nothing else in the stack acts on manifest.machine.workspace,
// so without this the directory never exists in the guest and the field has no
// observable effect.
func scriptPrelude(f *Fusefile) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString("if (set -o pipefail) 2>/dev/null; then set -o pipefail; fi\n")
	workspace := workspaceOf(f)
	fmt.Fprintf(&b, "mkdir -p %s\n", shellQuote(workspace))
	fmt.Fprintf(&b, "cd %s\n", shellQuote(workspace))
	return b.String()
}

// compileBuildScript renders the file block and the setup steps, without the
// run command. an empty setup phase yields "" rather than a bare prelude:
// `fuse build` exists to bake setup, and files alone are re-materialized on
// every boot anyway, so there is nothing to bake.
//
// after the file block it emits exactly the fragments layer keys are derived
// from, so a `fuse build` runs the same bytes the plan hashed. the files
// themselves are folded into the base key instead, since they are part of the
// state the first step builds on rather than a step of their own.
func compileBuildScript(f *Fusefile) string {
	if len(f.Setup) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(scriptPrelude(f))
	b.WriteString(renderFiles(f.Files))
	for _, script := range setupScripts(f) {
		b.WriteString(script)
		b.WriteString("\n")
	}
	return b.String()
}

// SetupScriptRange renders setup steps [from, to) as one runnable script.
//
// It exists because `fuse build` runs the cacheable part of the setup phase one
// step at a time, snapshotting between them, rather than as the single blob
// compileBuildScript emits. Every one of those execs is a fresh shell, so every
// one of them needs the prelude again. Without it a step would run without
// strict mode and, far worse, in whatever directory the shell happened to start
// in rather than the workspace, which fails quietly rather than loudly.
//
// includeFiles writes the file block ahead of the steps, and belongs only on
// the first exec of a cold build. Files are folded into BaseKey, so any layer
// artifact the build seeded from already contains them.
//
// SetupScriptRange(f, 0, len(f.Setup), true) is compileBuildScript(f) byte for
// byte. That equivalence is what lets the stepped path and the single-blob path
// coexist without drifting, and there is a test pinning it.
func SetupScriptRange(f *Fusefile, from, to int, includeFiles bool) string {
	scripts := setupScripts(f)
	if from < 0 {
		from = 0
	}
	if to > len(scripts) {
		to = len(scripts)
	}
	if from >= to && !includeFiles {
		return ""
	}
	var b strings.Builder
	b.WriteString(scriptPrelude(f))
	if includeFiles {
		b.WriteString(renderFiles(f.Files))
	}
	for _, script := range scripts[from:to] {
		b.WriteString(script)
		b.WriteString("\n")
	}
	return b.String()
}

// compileRunScript renders the file block and the run command, without the
// setup steps.
//
// Files are re-materialized here even though a `fuse build` artifact already
// carries the copy written during the bake: they are authoring inputs that may
// have been edited since, and rewriting a few KiB is cheap. Setup is the
// expensive work --from-build exists to skip; files are not.
func compileRunScript(f *Fusefile) string {
	if f.Run.IsEmpty() && len(f.Files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(scriptPrelude(f))
	b.WriteString(renderFiles(f.Files))
	if !f.Run.IsEmpty() {
		b.WriteString(f.Run.Render())
		b.WriteString("\n")
	}
	return b.String()
}

// compileStartupScript joins the setup steps and the run command into a single
// shell script with a strict-mode prelude. if there is nothing to run (no
// setup steps and no run command), it returns "" rather than a bare prelude.
func compileStartupScript(f *Fusefile) string {
	if len(f.Setup) == 0 && f.Run.IsEmpty() && len(f.Files) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(scriptPrelude(f))
	b.WriteString(renderFiles(f.Files))
	for _, script := range setupScripts(f) {
		b.WriteString(script)
		b.WriteString("\n")
	}
	if !f.Run.IsEmpty() {
		b.WriteString(f.Run.Render())
		b.WriteString("\n")
	}
	return b.String()
}

// StartupScriptFrom renders the boot script for an environment seeded from a
// layer artifact that already covers setup steps [0, from).
//
// It is compileStartupScript with a head start: the steps the artifact baked in
// are dropped, and everything after them still runs, followed by the run
// command. from == 0 makes it compileStartupScript exactly, which is the
// no-cache-hit case and is pinned by a test.
//
// Files are rewritten even though the artifact already carries the copy made
// when it was baked, for the same reason compileRunScript rewrites them: they
// are authoring inputs that may have been edited since the bake, and a few KiB
// is cheap. Setup is the expensive work a cache hit exists to skip; files are
// not.
func StartupScriptFrom(f *Fusefile, from int) string {
	if from <= 0 {
		return compileStartupScript(f)
	}
	scripts := setupScripts(f)
	if from > len(scripts) {
		from = len(scripts)
	}
	if from == len(scripts) && f.Run.IsEmpty() && len(f.Files) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(scriptPrelude(f))
	b.WriteString(renderFiles(f.Files))
	for _, script := range scripts[from:] {
		b.WriteString(script)
		b.WriteString("\n")
	}
	if !f.Run.IsEmpty() {
		b.WriteString(f.Run.Render())
		b.WriteString("\n")
	}
	return b.String()
}

// parseSize parses a size string ("512MB", "2G", "1.5GiB") into MiB. every
// unit is binary. an empty string is not an error; it means the field was
// omitted and yields zero.
//
// a size that does not land on a whole MiB is rejected rather than truncated:
// silently handing back less memory than asked for is worse than an error the
// author can act on.
func parseSize(s string) (int32, error) {
	if s == "" {
		return 0, nil
	}

	m := sizePattern.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf(
			"invalid size %q (expected a number and a unit, e.g. \"512MB\", \"2GB\", \"1.5GiB\")", s)
	}

	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf(
			"invalid size %q (expected a number and a unit, e.g. \"512MB\", \"2GB\", \"1.5GiB\")", s)
	}

	// compute in float64 so a decimal like "1.5GB" is exact for the halves
	// and quarters authors actually write, then insist on a whole MiB.
	mb := n * float64(sizeUnitsMiB[m[2]])
	if mb != math.Trunc(mb) {
		return 0, fmt.Errorf("invalid size %q: must be a whole number of MiB", s)
	}
	// a well-formed but huge value (e.g. "2097152GB") would otherwise
	// overflow int32 silently and wrap to a negative size.
	if mb > math.MaxInt32 {
		return 0, fmt.Errorf("invalid size %q: value too large", s)
	}
	return int32(mb), nil
}
