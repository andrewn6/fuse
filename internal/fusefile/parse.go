package fusefile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse decodes a Fusefile from yaml bytes using a strict decoder (unknown
// fields are rejected) and then validates the result. It returns the parsed
// Fusefile only if it is structurally valid.
func Parse(data []byte) (*Fusefile, error) {
	f, err := Decode(data)
	if err != nil {
		return nil, err
	}
	if err := Validate(f); err != nil {
		return nil, err
	}
	return f, nil
}

// Decode decodes a Fusefile from yaml with a strict decoder without validating
// it. Parse is the normal entry point; Decode is separate so a caller that
// wants to report structural and compile problems together (`fuse validate`)
// can run Validate and Compile over the same decoded file.
func Decode(data []byte) (*Fusefile, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var f Fusefile
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse fusefile: %w", err)
	}

	// a Fusefile is exactly one yaml document. only the first is decoded, so
	// everything after a stray `---` would vanish silently; say so instead.
	var extra yaml.Node
	err := dec.Decode(&extra)
	switch {
	case err == nil:
		return nil, fmt.Errorf("parse fusefile: must contain exactly one yaml document, found more than one")
	case !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("parse fusefile: %w", err)
	}

	return &f, nil
}

// Validate reports every structural rule violation in f, joined into a single
// error.
func Validate(f *Fusefile) error { return validate(f) }

// validate checks structural rules that yaml decoding alone cannot enforce.
// all violations are collected and returned together (via errors.Join) so a
// caller sees every problem in one pass instead of one error at a time.
//
// map iteration order in go is randomized, so service names and env keys are
// sorted before validating; this keeps the joined error message deterministic
// across runs.
func validate(f *Fusefile) error {
	var errs []error

	if f.Version != 1 {
		errs = append(errs, fmt.Errorf("version: must be 1"))
	}

	// placement labels: keys are sorted first so the joined message is stable
	// regardless of map iteration order.
	labelKeys := make([]string, 0, len(f.Placement.Labels))
	for key := range f.Placement.Labels {
		labelKeys = append(labelKeys, key)
	}
	sort.Strings(labelKeys)

	for _, key := range labelKeys {
		if !ValidLabel(key) {
			errs = append(errs, fmt.Errorf("placement.labels: invalid label key %q", key))
		}
		if value := f.Placement.Labels[key]; !ValidLabel(value) {
			errs = append(errs, fmt.Errorf("placement.labels.%s: invalid label value %q", key, value))
		}
	}

	for i, step := range f.Setup {
		if strings.TrimSpace(step.Run) == "" {
			errs = append(errs, fmt.Errorf("setup[%d].run: is required", i))
		}
		// a workdir is emitted as `cd <workdir>` inside the step's subshell,
		// under the same reasoning as workspace below: shellQuote stops it from
		// altering the script, it does not stop a `..` from landing somewhere
		// the author did not mean. a relative path is fine here, unlike
		// workspace, because it resolves against a known directory.
		if wd := step.Workdir; wd != "" {
			switch {
			case strings.ContainsAny(wd, "\x00\n"):
				errs = append(errs, fmt.Errorf("setup[%d].workdir: must not contain newlines or NUL bytes", i))
			case containsDotDot(wd):
				errs = append(errs, fmt.Errorf("setup[%d].workdir: must not contain %q segments, got %q", i, "..", wd))
			}
		}
		if !step.cacheable() && len(step.Inputs) > 0 {
			errs = append(errs, fmt.Errorf("setup[%d].inputs: not allowed on a step with cache: false", i))
		}
		for j, in := range step.Inputs {
			switch {
			case strings.TrimSpace(in) == "":
				errs = append(errs, fmt.Errorf("setup[%d].inputs[%d]: must not be empty", i, j))
			case path.IsAbs(in) || filepath.IsAbs(in):
				errs = append(errs, fmt.Errorf("setup[%d].inputs[%d]: must be relative to the Fusefile, got %q", i, j, in))
			case escapesRoot(in):
				errs = append(errs, fmt.Errorf("setup[%d].inputs[%d]: must not traverse outside the Fusefile's directory, got %q", i, j, in))
			}
		}
	}

	serviceNames := make([]string, 0, len(f.Services))
	for name := range f.Services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)

	for _, name := range serviceNames {
		svc := f.Services[name]

		if svc.Image == "" {
			errs = append(errs, fmt.Errorf("services.%s: image is required", name))
		}

		for i, port := range svc.Ports {
			if port < 1 || port > 65535 {
				errs = append(errs, fmt.Errorf("services.%s.ports[%d]: must be between 1 and 65535", name, i))
			}
		}

		envKeys := make([]string, 0, len(svc.Env))
		for key := range svc.Env {
			envKeys = append(envKeys, key)
		}
		sort.Strings(envKeys)

		for _, key := range envKeys {
			if strings.TrimSpace(key) == "" {
				errs = append(errs, fmt.Errorf("services.%s.env: environment variable name must not be empty", name))
				continue
			}
			env := svc.Env[key]
			switch {
			case env.Value != "" && env.Secret != "":
				errs = append(errs, fmt.Errorf("services.%s.env.%s: value and secret are mutually exclusive", name, key))
			case env.Value == "" && env.Secret == "":
				errs = append(errs, fmt.Errorf("services.%s.env.%s: value or secret is required", name, key))
			}
		}
	}

	// Which body a file carries is checked here rather than at compile time:
	// ResolveFiles folds Source into Content in between, after which the two
	// cases are indistinguishable.
	for i, file := range f.Files {
		switch {
		case file.Source != "" && file.Content != "":
			errs = append(errs, fmt.Errorf("files[%d]: source and content are mutually exclusive", i))
		case file.Source == "" && file.Content == "":
			errs = append(errs, fmt.Errorf("files[%d]: source or content is required", i))
		}
	}

	// the workspace is emitted into the generated script as `mkdir -p <ws>`
	// followed by `cd <ws>`. shellQuote keeps it from altering the script, but
	// a relative or traversing path still lands somewhere the author did not
	// mean, relative to whatever directory the guest agent happens to start in.
	if ws := f.Workspace; ws != "" {
		switch {
		case strings.ContainsAny(ws, "\x00\n"):
			errs = append(errs, fmt.Errorf("workspace: must not contain newlines or NUL bytes"))
		case !path.IsAbs(ws):
			errs = append(errs, fmt.Errorf("workspace: must be an absolute path, got %q", ws))
		case containsDotDot(ws):
			errs = append(errs, fmt.Errorf("workspace: must not contain %q segments, got %q", "..", ws))
		}
	}

	seenPorts := make(map[int]int, len(f.Expose))
	seenNames := make(map[string]int, len(f.Expose))
	for i, exp := range f.Expose {
		if exp.Port < 1 || exp.Port > 65535 {
			errs = append(errs, fmt.Errorf("expose[%d].port: must be between 1 and 65535", i))
		} else if prev, dup := seenPorts[exp.Port]; dup {
			errs = append(errs, fmt.Errorf("expose[%d].port: %d is already exposed by expose[%d]", i, exp.Port, prev))
		} else {
			seenPorts[exp.Port] = i
		}

		if exp.As == "" {
			continue
		}
		if !ValidExposeName(exp.As) {
			errs = append(errs, fmt.Errorf(
				"expose[%d].as: invalid name %q (lowercase letters, digits and dashes, 63 chars max)", i, exp.As))
		}
		if prev, dup := seenNames[exp.As]; dup {
			errs = append(errs, fmt.Errorf("expose[%d].as: %q is already used by expose[%d]", i, exp.As, prev))
		} else {
			seenNames[exp.As] = i
		}
	}

	// an empty secret name is a requirement no `--secret` flag can satisfy, and
	// the server's ExtractRequiredSecrets skips empty refs, so the two sides
	// would disagree about what the environment needs.
	for i, s := range f.Secrets {
		if strings.TrimSpace(s) == "" {
			errs = append(errs, fmt.Errorf("secrets[%d]: must not be empty", i))
		}
	}

	return errors.Join(errs...)
}

// containsDotDot reports whether any segment of a slash-separated guest path
// is "..".
func containsDotDot(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// escapesRoot reports whether a relative input path climbs out of the
// directory it is resolved against. slashes are normalized first so a
// windows-authored `..\x` is caught too.
func escapesRoot(p string) bool {
	clean := path.Clean(filepath.ToSlash(p))
	return clean == ".." || strings.HasPrefix(clean, "../")
}
