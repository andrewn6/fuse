package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Secrets validation limits.
const (
	MaxSecretKeys       = 256
	MaxSecretKeyLength  = 256
	MaxSecretValueSize  = 65536   // 64 KB per value
	MaxSecretsTotalSize = 1048576 // 1 MB aggregate
)

// ErrSecretsValidation is returned when the provided secrets fail
// validation against the manifest or size constraints.
var ErrSecretsValidation = errors.New("secrets validation failed")

// manifestSecretExtractor is a minimal struct that unmarshals only the
// secret references from a compiled manifest. It ignores everything
// else — no dependency on fused's manifest package.
//
// COUPLING NOTE: the manifest-shaped secret cross-check below
// (machine -> env -> secret, services -> env -> secret) embeds fused's
// manifest contract. It is a fused-profile validation, not a hard requirement
// of the generic create path; a non-fused agent profile with a different
// manifest schema would supply its own validation. Flagged per the decoupling
// scan; no behavioral change.
type manifestSecretExtractor struct {
	// Machine.Env is the machine-wide env block, applied to the startup
	// script. A ref there is as required as a service's: the orchestrator
	// resolves it into /fuse/env at boot, so a missing value would silently
	// become an empty variable.
	Machine struct {
		Env map[string]struct {
			Secret *string `json:"secret"`
		} `json:"env"`
	} `json:"machine"`
	Services map[string]struct {
		Env map[string]struct {
			Secret *string `json:"secret"`
		} `json:"env"`
	} `json:"services"`
}

// ExtractRequiredSecrets parses a compiled manifest JSON and returns the set
// of secret names it references via env vars, machine-wide and per service.
func ExtractRequiredSecrets(manifestJSON []byte) (map[string]bool, error) {
	var m manifestSecretExtractor
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return nil, fmt.Errorf("parse manifest for secret refs: %w", err)
	}

	required := make(map[string]bool)
	for _, v := range m.Machine.Env {
		if v.Secret != nil && *v.Secret != "" {
			required[*v.Secret] = true
		}
	}
	for _, svc := range m.Services {
		for _, v := range svc.Env {
			if v.Secret != nil && *v.Secret != "" {
				required[*v.Secret] = true
			}
		}
	}
	return required, nil
}

// ValidateSecrets checks that the provided secrets map satisfies size
// constraints and that all manifest-required secrets are present.
// Returns a wrapped ErrSecretsValidation on failure.
func ValidateSecrets(manifestJSON []byte, secrets map[string]string) error {
	var issues []string

	// Key count.
	if len(secrets) > MaxSecretKeys {
		issues = append(issues, fmt.Sprintf("too many secrets: %d (max %d)", len(secrets), MaxSecretKeys))
	}

	// Per-key and per-value limits.
	var totalSize int
	for k, v := range secrets {
		if len(k) > MaxSecretKeyLength {
			issues = append(issues, fmt.Sprintf("secret key %q exceeds max length %d", k, MaxSecretKeyLength))
		}
		if len(v) > MaxSecretValueSize {
			issues = append(issues, fmt.Sprintf("secret %q value exceeds max size %d bytes", k, MaxSecretValueSize))
		}
		totalSize += len(v)
	}
	if totalSize > MaxSecretsTotalSize {
		issues = append(issues, fmt.Sprintf("total secrets size %d bytes exceeds max %d", totalSize, MaxSecretsTotalSize))
	}

	// Cross-check against manifest.
	required, err := ExtractRequiredSecrets(manifestJSON)
	if err != nil {
		issues = append(issues, err.Error())
	} else {
		var missing []string
		for name := range required {
			if _, ok := secrets[name]; !ok {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			issues = append(issues, fmt.Sprintf("missing required secrets: %s", strings.Join(missing, ", ")))
		}
	}

	if len(issues) > 0 {
		sort.Strings(issues)
		return fmt.Errorf("%w: %s", ErrSecretsValidation, strings.Join(issues, "; "))
	}
	return nil
}

// SecretKeyNames returns sorted key names from a secrets map.
// Used for audit logging — never log values.
func SecretKeyNames(secrets map[string]string) []string {
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RedactSecretValues replaces occurrences of secret values in msg with
// [REDACTED]. Only redacts values of 8+ characters to avoid false
// positives on short strings. Processes longest values first to
// prevent partial matches.
func RedactSecretValues(msg string, secrets map[string]string) string {
	if len(secrets) == 0 || msg == "" {
		return msg
	}

	// Collect values worth redacting, sorted longest-first.
	vals := make([]string, 0, len(secrets))
	for _, v := range secrets {
		if len(v) >= 8 {
			vals = append(vals, v)
		}
	}
	sort.Slice(vals, func(i, j int) bool {
		return len(vals[i]) > len(vals[j])
	})

	for _, v := range vals {
		msg = strings.ReplaceAll(msg, v, "[REDACTED]")
	}
	return msg
}
