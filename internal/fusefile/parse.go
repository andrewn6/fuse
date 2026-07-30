package fusefile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"

	"gopkg.in/yaml.v3"
)

// Parse decodes a Fusefile from yaml bytes using a strict decoder (unknown
// fields are rejected) and then validates the result. It returns the parsed
// Fusefile only if it is structurally valid.
func Parse(data []byte) (*Fusefile, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var f Fusefile
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse fusefile: %w", err)
	}

	if err := validate(&f); err != nil {
		return nil, err
	}

	return &f, nil
}

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

		envKeys := make([]string, 0, len(svc.Env))
		for key := range svc.Env {
			envKeys = append(envKeys, key)
		}
		sort.Strings(envKeys)

		for _, key := range envKeys {
			env := svc.Env[key]
			switch {
			case env.Value != "" && env.Secret != "":
				errs = append(errs, fmt.Errorf("services.%s.env.%s: value and secret are mutually exclusive", name, key))
			case env.Value == "" && env.Secret == "":
				errs = append(errs, fmt.Errorf("services.%s.env.%s: value or secret is required", name, key))
			}
		}
	}

	for i, exp := range f.Expose {
		if exp.Port < 1 || exp.Port > 65535 {
			errs = append(errs, fmt.Errorf("expose[%d].port: must be between 1 and 65535", i))
		}
	}

	return errors.Join(errs...)
}
