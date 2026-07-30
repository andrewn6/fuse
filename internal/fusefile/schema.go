// package fusefile is the canonical authoring format for a fuse environment.
// a Fusefile is parsed and compiled client-side into the orchestrator wire
// (CreateEnvironmentRequest); the orchestrator never sees a Fusefile.
package fusefile

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Fusefile is the v1 authoring contract.
type Fusefile struct {
	Version   int                `yaml:"version"`
	Image     string             `yaml:"image,omitempty"`
	Resources Resources          `yaml:"resources,omitempty"`
	Placement Placement          `yaml:"placement,omitempty"`
	Cache     Cache              `yaml:"cache,omitempty"`
	Setup     []Step             `yaml:"setup,omitempty"`
	Services  map[string]Service `yaml:"services,omitempty"`
	Run       string             `yaml:"run,omitempty"`
	Workspace string             `yaml:"workspace,omitempty"`
	Expose    []Expose           `yaml:"expose,omitempty"`
	Secrets   []string           `yaml:"secrets,omitempty"`
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
// which adds inputs and cache.
//
// Cache is a pointer so "unset" is distinguishable from an explicit
// "cache: false": a step that reads secrets or writes outside the rootfs must
// opt out, and an unset field must not be read as an opt-out.
type Step struct {
	Run    string   `yaml:"run"`
	Inputs []string `yaml:"inputs,omitempty"`
	Cache  *bool    `yaml:"cache,omitempty"`
}

// stepFields is the set of keys the mapping form accepts. Parse's decoder runs
// with KnownFields(true), but a custom UnmarshalYAML decodes through a
// yaml.Node and does not inherit that, so unknown keys are rejected here or
// `- ruh: npm ci` would silently parse as an empty step.
var stepFields = map[string]bool{"run": true, "inputs": true, "cache": true}

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

// Resources is the human-friendly hardware spec; compiled to ResourceSpec.
type Resources struct {
	CPUs    int32  `yaml:"cpus,omitempty"`
	GPU     int    `yaml:"gpu,omitempty"`      // device count: whole GPUs, or MIG instances when gpu_profile is set
	GPUKind string `yaml:"gpu_kind,omitempty"` // optional match, e.g. "a100"
	// GPUProfile requests fractional GPU allocation: a MIG profile in
	// nvidia mig-parted vocabulary (e.g. "1g.10gb", "2g.20gb"). When set,
	// `gpu` counts MIG instances of this profile rather than whole
	// devices (decision D5). Empty means whole-device allocation.
	GPUProfile string `yaml:"gpu_profile,omitempty"`
	Memory     string `yaml:"memory,omitempty"`      // e.g. "2GB"
	Storage    string `yaml:"storage,omitempty"`     // e.g. "10GB"
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
