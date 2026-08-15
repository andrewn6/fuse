// package fusefile is the canonical authoring format for a fuse environment.
// a Fusefile is parsed and compiled client-side into the orchestrator wire
// (CreateEnvironmentRequest); the orchestrator never sees a Fusefile.
package fusefile

import (
	"fmt"
	"math"

	"gopkg.in/yaml.v3"
)

// Fusefile is the v1 authoring contract.
type Fusefile struct {
	Version   int                `yaml:"version"`
	Image     string             `yaml:"image,omitempty"`
	Resources Resources          `yaml:"resources,omitempty"`
	Placement Placement          `yaml:"placement,omitempty"`
	Cache     Cache              `yaml:"cache,omitempty"`
	Files     []File             `yaml:"files,omitempty"`
	Setup     []Step             `yaml:"setup,omitempty"`
	Services  map[string]Service `yaml:"services,omitempty"`
	Run       Command            `yaml:"run,omitempty"`
	Workspace string             `yaml:"workspace,omitempty"`
	Expose    []Expose           `yaml:"expose,omitempty"`
	Secrets   []string           `yaml:"secrets,omitempty"`

	// Healthcheck is the environment-level readiness probe. It is optional
	// and there is at most one: the point of the block is that the whole
	// sandbox has a single verdict.
	Healthcheck *HealthProbe `yaml:"healthcheck,omitempty"`

	// StartupTimeout bounds the generated startup script (setup + run) as a
	// go duration, e.g. "45s". Empty means the orchestrator's default. The
	// orchestrator rejects a value above its configured ceiling rather than
	// clamping it, so an author always knows what bound they got.
	//
	// This is not the knob for a genuinely long setup phase: the ceiling is
	// bounded by the control plane's HTTP write timeout because create is
	// synchronous. Bake long work into an image with `fuse build` instead.
	StartupTimeout string `yaml:"startup_timeout,omitempty"`
}

// Command is the polymorphic `run` field. It accepts either a scalar shell
// command (the legacy form, interpreted by sh -lc byte for byte) or a list of
// strings (the exec form, each element shell-quoted into a single command line
// so no argument is subject to word splitting, globbing, or metacharacter
// reinterpretation). Exactly one of Shell or Argv is populated.
type Command struct {
	// Shell is the shell-form command; set when `run` was a yaml scalar.
	Shell string
	// Argv is the exec-form argv; set when `run` was a yaml sequence.
	Argv []string
}

// UnmarshalYAML accepts either a scalar (shell form) or a sequence of strings
// (exec form). KnownFields(true) does not propagate into a custom unmarshaler,
// so other node kinds -- including a mapping, which would look like an object
// form this type does not have -- are rejected here rather than silently
// dropped. An empty sequence is a decode error: an argv of zero arguments is
// not a command. A non-string element (an int, bool, float, or null scalar) is
// a decode error too: yaml.v3 silently coerces such scalars into strings when
// decoding into []string, so each element's tag is checked explicitly.
func (c *Command) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var shell string
		if err := node.Decode(&shell); err != nil {
			return err
		}
		c.Shell = shell
		c.Argv = nil
		return nil
	case yaml.SequenceNode:
		if len(node.Content) == 0 {
			return fmt.Errorf("run: an empty list is not a valid command")
		}
		argv := make([]string, 0, len(node.Content))
		for i, el := range node.Content {
			// yaml.v3 silently coerces a non-string scalar (5, true, 1.5, null)
			// into a string when decoding into []string, so the tag is checked
			// explicitly: only genuine string scalars are accepted.
			if el.Kind != yaml.ScalarNode || el.Tag != "!!str" {
				return fmt.Errorf("run: argument %d is not a string (tag %s)", i, el.Tag)
			}
			var s string
			if err := el.Decode(&s); err != nil {
				return fmt.Errorf("run: argument %d is not a string: %w", i, err)
			}
			argv = append(argv, s)
		}
		c.Shell = ""
		c.Argv = argv
		return nil
	default:
		return fmt.Errorf("run: must be a string or a list of strings")
	}
}

// MarshalYAML renders the command back to its source shape: a scalar for the
// shell form, a sequence for the exec form. This keeps a round-trip through the
// custom unmarshaler lossless (e.g. `fuse init`'s scaffold).
func (c Command) MarshalYAML() (any, error) {
	if len(c.Argv) > 0 {
		return c.Argv, nil
	}
	return c.Shell, nil
}

// File is one file materialized inside the guest before setup runs.
//
// Exactly one of Source (a path on the authoring machine, resolved relative to
// the Fusefile's directory by ResolveFiles) and Content (a literal) is set.
// Content is what the compiler emits; Source is sugar that ResolveFiles reads
// into Content, which keeps Compile a pure function of the Fusefile.
//
// This is a transport for config and small code, not for model weights or
// datasets: entries are base64-encoded into the startup script, which travels
// in the create request body, so MaxFilesBytes caps their total size. Fetch
// large artifacts from inside `setup` instead.
type File struct {
	// Path is where the file lands in the guest. A relative path resolves
	// against the workspace.
	Path string `yaml:"path"`
	// Source is a path on the authoring machine, relative to the Fusefile.
	Source string `yaml:"source,omitempty"`
	// Content is a literal file body.
	Content string `yaml:"content,omitempty"`
	// Mode is an octal permission string, e.g. "0755". Empty leaves the
	// mode to the guest's umask.
	Mode string `yaml:"mode,omitempty"`
}

// Placement constrains which host in a self-hosted fleet may run the
// environment. Every field is a hard gate, not a preference: a request that
// matches no host is rejected immediately, never queued.
//
// Region is deliberately absent; it lives under Resources for historical
// reasons and a second spelling of the same selector would be worse than the
// block split.
type Placement struct {
	// Host pins the environment to an exact host id. A pin is not an
	// override: the pinned host still has to be active, run the right
	// backend, and fit the request.
	Host string `yaml:"host,omitempty"`

	// Labels must all match the host's operator-declared labels (AND).
	Labels map[string]string `yaml:"labels,omitempty"`
}

// Cache is the top-level opt-in for the setup layer cache. caching is off by
// default: a layer is a rootfs captured mid-provisioning, so opting in is a
// deliberate act.
type Cache struct {
	Enabled bool `yaml:"enabled,omitempty"`
}

// Step is one setup step. it accepts two yaml forms: a bare scalar
// ("apt-get update -qq"), equivalent to {run: ...} and unchanged from v1's
// list of strings; and a mapping ({run: npm ci, inputs: [package.json]}),
// which adds inputs, cache, and workdir.
//
// Cache is a pointer so "unset" is distinguishable from an explicit
// "cache: false": a step that reads secrets or writes outside the rootfs must
// opt out, and an unset field must not be read as an opt-out.
type Step struct {
	Run    string   `yaml:"run"`
	Inputs []string `yaml:"inputs,omitempty"`
	Cache  *bool    `yaml:"cache,omitempty"`

	// Workdir scopes this one step to a directory. a relative path resolves
	// against Fusefile.Workspace, since every step starts there. the step is
	// emitted as a subshell, so the directory change does not leak into the
	// next step; only the directory is scoped, not the shell (see the note on
	// setupScripts). empty means the step runs in the workspace, unchanged.
	Workdir string `yaml:"workdir,omitempty"`
}

// stepFields is the set of keys the mapping form accepts. Parse's decoder runs
// with KnownFields(true), but a custom UnmarshalYAML decodes through a
// yaml.Node and does not inherit that, so unknown keys are rejected here or
// `- ruh: npm ci` would silently parse as an empty step.
var stepFields = map[string]bool{"run": true, "inputs": true, "cache": true, "workdir": true}

// UnmarshalYAML decodes either a bare scalar (the legacy form, kept working
// byte for byte) or the mapping form.
func (s *Step) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var run string
		if err := node.Decode(&run); err != nil {
			return err
		}
		s.Run = run
		return nil
	}

	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("setup step: must be a string or a mapping")
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !stepFields[key] {
			return fmt.Errorf("line %d: field %s not found in setup step", node.Content[i].Line, key)
		}
	}

	// step is an alias without the method set, so decoding it does not recurse
	// back into UnmarshalYAML.
	type step Step
	var raw step
	if err := node.Decode(&raw); err != nil {
		return err
	}
	*s = Step(raw)
	return nil
}

// VCPUs is a whole vCPU count. It accepts a yaml integer or a whole-valued
// float, so `cpus: 2` and `cpus: 2.0` both mean two vCPUs, and rejects a
// genuine fraction: firecracker takes an integer vcpu_count, qemu an integer
// -smp, and neither host agent writes a cgroup quota, so there is nothing in
// the stack that could honor `cpus: 0.5`.
type VCPUs int32

// UnmarshalYAML decodes the vCPU count through float64 so a whole-valued float
// is accepted and a fraction is rejected with a reason.
func (c *VCPUs) UnmarshalYAML(node *yaml.Node) error {
	var n float64
	if err := node.Decode(&n); err != nil {
		return fmt.Errorf("cpus: invalid %q (expected a whole number of vcpus, e.g. 2)", node.Value)
	}
	if n != math.Trunc(n) {
		return fmt.Errorf(
			"cpus: %v is not a whole number of vcpus (firecracker and qemu both take an integer vcpu count; fractional cpus are not supported)",
			n)
	}
	if n > math.MaxInt32 || n < math.MinInt32 {
		return fmt.Errorf("cpus: %v is out of range", n)
	}
	*c = VCPUs(n)
	return nil
}

// Resources is the human-friendly hardware spec; compiled to ResourceSpec.
type Resources struct {
	CPUs    VCPUs  `yaml:"cpus,omitempty"`
	GPU     int    `yaml:"gpu,omitempty"`      // device count: whole GPUs, or MIG instances when gpu_profile is set
	GPUKind string `yaml:"gpu_kind,omitempty"` // optional match, e.g. "a100"
	// GPUProfile requests fractional GPU allocation: a MIG profile in
	// nvidia mig-parted vocabulary (e.g. "1g.10gb", "2g.20gb"). When set,
	// `gpu` counts MIG instances of this profile rather than whole
	// devices (decision D5). Empty means whole-device allocation.
	GPUProfile string `yaml:"gpu_profile,omitempty"`
	Memory     string `yaml:"memory,omitempty"` // e.g. "2GB", "2G", "2GiB", "512MB"
	// Disk sizes the guest root disk, e.g. "10GB". It is the preferred
	// spelling; Storage is a permanent alias kept for files written before
	// the rename. Setting both is allowed only if they mean the same size.
	Disk       string `yaml:"disk,omitempty"`
	Storage    string `yaml:"storage,omitempty"`     // alias for Disk, never removed
	Region     string `yaml:"region,omitempty"`      // schedules only onto a host registered in this region; empty matches any
	MaxRuntime string `yaml:"max_runtime,omitempty"` // go duration
	// IdleTimeout destroys the environment after this long with no exec
	// and no attach session. Go duration. "idle" means exactly that: no
	// control-plane activity. In-guest CPU or network traffic is not
	// observed. Empty means no idle expiry.
	IdleTimeout string `yaml:"idle_timeout,omitempty"` // go duration
}

// Service is one in-vm service; compiled to manifest.services and a compose unit.
type Service struct {
	Image string              `yaml:"image"`
	Ports []int               `yaml:"ports,omitempty"`
	Env   map[string]EnvValue `yaml:"env,omitempty"`

	// Command, Restart, HealthCheck, and DependsOn map one-to-one onto their
	// compose-native counterparts; the guest's `docker compose up` (decision
	// D1) is what interprets them, so no new runtime semantics are
	// introduced here. They govern the compose container, not the VM: a
	// failing healthcheck marks the service unhealthy inside the guest, it
	// does not signal the orchestrator that the environment is dead.
	Command []string `yaml:"command,omitempty"`
	// Restart must be one of the compose-native policies: "no", "always",
	// "on-failure", "unless-stopped".
	Restart     string       `yaml:"restart,omitempty"`
	HealthCheck *HealthCheck `yaml:"healthcheck,omitempty"`
	DependsOn   []string     `yaml:"depends_on,omitempty"`
}

// HealthCheck is a compose-native container healthcheck. Interval and Timeout
// are go durations (e.g. "10s").
type HealthCheck struct {
	Test     []string `yaml:"test,omitempty"`
	Interval string   `yaml:"interval,omitempty"`
	Timeout  string   `yaml:"timeout,omitempty"`
	Retries  int      `yaml:"retries,omitempty"`
}

// HealthProbe is the environment-level readiness probe: one verdict for
// whether the sandbox as a whole is doing its job, visible to the control
// plane.
//
// It is deliberately NOT the service-level HealthCheck above, and the two are
// not interchangeable. That one is compose-native, governs a single container,
// and is evaluated by the guest's `docker compose`; a container it marks
// unhealthy never signals the orchestrator. This one is evaluated by the guest
// agent (fused) over the environment as a whole, and its verdict travels back
// to the fleet, which is what makes `fuse up --wait-healthy` mean anything.
//
// Exactly one of HTTP and Exec is set. Two probes would produce two verdicts,
// and a block whose whole purpose is a single answer must not have to
// reconcile them.
type HealthProbe struct {
	// HTTP probes a port inside the guest. Successful means a response with
	// a 2xx or 3xx status.
	HTTP *HealthProbeHTTP `yaml:"http,omitempty"`

	// Exec is an argv run inside the guest. Successful means exit status 0.
	// It is an argv rather than a shell string so an argument can never be
	// reinterpreted by a shell, matching the exec form of `run`.
	Exec []string `yaml:"exec,omitempty"`

	// Interval is how long the guest agent waits between attempts, as a go
	// duration ("5s"). Empty means the guest agent's default.
	Interval string `yaml:"interval,omitempty"`

	// Timeout bounds one attempt, as a go duration ("2s"). Empty means the
	// guest agent's default. It may not exceed Interval: attempts are run one
	// at a time, so a timeout longer than the interval would silently stretch
	// the interval instead of overlapping attempts.
	Timeout string `yaml:"timeout,omitempty"`

	// Retries is how many consecutive failed attempts flip the verdict from
	// passing to failing. Zero means the guest agent's default. A single
	// failed attempt is not a verdict: a probe that is retried is the only
	// kind worth reporting on.
	Retries int `yaml:"retries,omitempty"`

	// StartPeriod is a grace window measured from the first attempt during
	// which failures do not count against Retries, as a go duration ("10s").
	// It covers the gap between "the process was started" and "the process
	// is serving", which is exactly the gap this probe exists to close. Once
	// the probe passes once, the window is over for good: a service that came
	// up and then fell over is failing, not still starting.
	StartPeriod string `yaml:"start_period,omitempty"`
}

// HealthProbeHTTP is an HTTP GET against a port inside the guest. The probe
// runs in-guest, so the port needs no `expose` entry: this asks whether the
// app is serving, not whether anyone outside can reach it.
type HealthProbeHTTP struct {
	// Port is the guest-side port to dial, 1 to 65535.
	Port int `yaml:"port"`
	// Path is the request path, which must start with "/". Empty means "/".
	Path string `yaml:"path,omitempty"`
}

// EnvValue is either a literal value or a secret reference. exactly one is set.
type EnvValue struct {
	Value  string `yaml:"value,omitempty"`
	Secret string `yaml:"secret,omitempty"`
}

// Expose publishes a guest port to the outside world.
type Expose struct {
	Port int    `yaml:"port"`
	As   string `yaml:"as,omitempty"`
}
