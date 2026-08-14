package secrets_test

import (
	"errors"
	"strings"
	"testing"

	secpkg "github.com/folsomintel/fuse/internal/secrets"
)

func TestExtractRequiredSecrets_empty(t *testing.T) {
	manifest := []byte(`{"version":"1","services":{}}`)
	got, err := secpkg.ExtractRequiredSecrets(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 secrets, got %d", len(got))
	}
}

func TestExtractRequiredSecrets_findsSecretRefs(t *testing.T) {
	manifest := []byte(`{
		"version": "1",
		"services": {
			"api": {
				"env": {
					"DB_PASSWORD": {"secret": "db_password"},
					"API_KEY": {"secret": "api_key"},
					"PORT": {"literal": "8080"}
				}
			},
			"worker": {
				"env": {
					"DB_PASSWORD": {"secret": "db_password"},
					"REDIS_URL": {"literal": "redis://localhost"}
				}
			}
		}
	}`)

	got, err := secpkg.ExtractRequiredSecrets(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 unique secrets, got %d: %v", len(got), got)
	}
	if !got["db_password"] {
		t.Error("missing db_password")
	}
	if !got["api_key"] {
		t.Error("missing api_key")
	}
}

// a ref in the machine-wide env block is as required as a service's: the
// orchestrator resolves it into /fuse/env at boot, so a value missing here
// would silently become an empty variable in the startup script.
func TestExtractRequiredSecrets_findsMachineEnvRefs(t *testing.T) {
	manifest := []byte(`{
		"version": "1",
		"machine": {
			"workspace": "/workspace",
			"env": {
				"DATABASE_URL": {"secret": "db_url"},
				"SHARED": {"secret": "db_password"},
				"NODE_ENV": {"value": "production"}
			}
		},
		"services": {
			"api": {"env": {"DB_PASSWORD": {"secret": "db_password"}}}
		}
	}`)

	got, err := secpkg.ExtractRequiredSecrets(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 unique secrets, got %d: %v", len(got), got)
	}
	if !got["db_url"] {
		t.Error("missing db_url from the machine env block")
	}
	if !got["db_password"] {
		t.Error("missing db_password")
	}
}

func TestExtractRequiredSecrets_noEnvField(t *testing.T) {
	manifest := []byte(`{
		"version": "1",
		"services": {
			"api": {
				"name": "api",
				"kind": "cmd",
				"run": ["sleep", "60"]
			}
		}
	}`)

	got, err := secpkg.ExtractRequiredSecrets(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0, got %d", len(got))
	}
}

func TestExtractRequiredSecrets_invalidJSON(t *testing.T) {
	_, err := secpkg.ExtractRequiredSecrets([]byte("not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidateSecrets_happyPath(t *testing.T) {
	manifest := []byte(`{
		"services": {
			"api": {
				"env": {
					"DB_PASSWORD": {"secret": "db_password"}
				}
			}
		}
	}`)

	secrets := map[string]string{
		"db_password": "hunter2",
	}

	if err := secpkg.ValidateSecrets(manifest, secrets); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateSecrets_missingRequired(t *testing.T) {
	manifest := []byte(`{
		"services": {
			"api": {
				"env": {
					"DB_PASSWORD": {"secret": "db_password"},
					"API_KEY": {"secret": "api_key"}
				}
			}
		}
	}`)

	secrets := map[string]string{
		"db_password": "hunter2",
		// api_key missing
	}

	err := secpkg.ValidateSecrets(manifest, secrets)
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
	if !errors.Is(err, secpkg.ErrSecretsValidation) {
		t.Errorf("expected secpkg.ErrSecretsValidation, got: %v", err)
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("error should mention missing key, got: %v", err)
	}
}

func TestValidateSecrets_extraSecretsAllowed(t *testing.T) {
	manifest := []byte(`{
		"services": {
			"api": {
				"env": {
					"DB_PASSWORD": {"secret": "db_password"}
				}
			}
		}
	}`)

	secrets := map[string]string{
		"db_password": "hunter2",
		"extra_key":   "extra_value", // not referenced by manifest
	}

	if err := secpkg.ValidateSecrets(manifest, secrets); err != nil {
		t.Fatalf("extra secrets should not cause error, got: %v", err)
	}
}

func TestValidateSecrets_tooManyKeys(t *testing.T) {
	manifest := []byte(`{"services":{}}`)
	secrets := make(map[string]string, secpkg.MaxSecretKeys+1)
	for i := 0; i <= secpkg.MaxSecretKeys; i++ {
		secrets[strings.Repeat("k", 4)+string(rune('a'+i%26))+string(rune('0'+i/26))] = "v"
	}

	err := secpkg.ValidateSecrets(manifest, secrets)
	if err == nil {
		t.Fatal("expected error for too many keys")
	}
	if !strings.Contains(err.Error(), "too many secrets") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateSecrets_keyTooLong(t *testing.T) {
	manifest := []byte(`{"services":{}}`)
	secrets := map[string]string{
		strings.Repeat("x", secpkg.MaxSecretKeyLength+1): "value",
	}

	err := secpkg.ValidateSecrets(manifest, secrets)
	if err == nil {
		t.Fatal("expected error for oversized key")
	}
	if !strings.Contains(err.Error(), "exceeds max length") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateSecrets_valueTooLarge(t *testing.T) {
	manifest := []byte(`{"services":{}}`)
	secrets := map[string]string{
		"big": strings.Repeat("x", secpkg.MaxSecretValueSize+1),
	}

	err := secpkg.ValidateSecrets(manifest, secrets)
	if err == nil {
		t.Fatal("expected error for oversized value")
	}
	if !strings.Contains(err.Error(), "exceeds max size") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateSecrets_totalTooLarge(t *testing.T) {
	manifest := []byte(`{"services":{}}`)
	// Create enough secrets to exceed total limit.
	secrets := make(map[string]string)
	perValue := secpkg.MaxSecretValueSize // just under individual limit
	count := (secpkg.MaxSecretsTotalSize / perValue) + 1
	for i := 0; i < count; i++ {
		secrets[strings.Repeat("k", 3)+string(rune('a'+i%26))] = strings.Repeat("v", perValue)
	}

	err := secpkg.ValidateSecrets(manifest, secrets)
	if err == nil {
		t.Fatal("expected error for total size exceeded")
	}
	if !strings.Contains(err.Error(), "total secrets size") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateSecrets_noSecretsNoManifestRefs(t *testing.T) {
	manifest := []byte(`{"version":"1","services":{"api":{"kind":"cmd","run":["sleep","60"]}}}`)
	if err := secpkg.ValidateSecrets(manifest, nil); err != nil {
		t.Fatalf("nil secrets with no refs should be ok, got: %v", err)
	}
	if err := secpkg.ValidateSecrets(manifest, map[string]string{}); err != nil {
		t.Fatalf("empty secrets with no refs should be ok, got: %v", err)
	}
}

func TestSecretKeyNames(t *testing.T) {
	secrets := map[string]string{
		"zebra": "z",
		"alpha": "a",
		"mango": "m",
	}
	got := secpkg.SecretKeyNames(secrets)
	if len(got) != 3 {
		t.Fatalf("expected 3, got %d", len(got))
	}
	if got[0] != "alpha" || got[1] != "mango" || got[2] != "zebra" {
		t.Errorf("not sorted: %v", got)
	}
}

func TestRedactSecretValues_replacesLongValues(t *testing.T) {
	secrets := map[string]string{
		"password": "super-secret-password-123",
	}
	msg := "error: connection failed with password super-secret-password-123 on host db"
	got := secpkg.RedactSecretValues(msg, secrets)
	if strings.Contains(got, "super-secret-password-123") {
		t.Errorf("secret value not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in output: %s", got)
	}
}

func TestRedactSecretValues_skipsShortValues(t *testing.T) {
	secrets := map[string]string{
		"port": "8080", // too short to redact (< 8 chars)
	}
	msg := "listening on port 8080"
	got := secpkg.RedactSecretValues(msg, secrets)
	if got != msg {
		t.Errorf("short values should not be redacted: %s", got)
	}
}

func TestRedactSecretValues_emptyInputs(t *testing.T) {
	if got := secpkg.RedactSecretValues("", nil); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	if got := secpkg.RedactSecretValues("hello", nil); got != "hello" {
		t.Errorf("expected hello, got %q", got)
	}
	if got := secpkg.RedactSecretValues("hello", map[string]string{}); got != "hello" {
		t.Errorf("expected hello, got %q", got)
	}
}

func TestRedactSecretValues_longestFirst(t *testing.T) {
	secrets := map[string]string{
		"short": "secret12",          // 8 chars
		"long":  "secret12-extended", // 17 chars, contains "secret12"
	}
	msg := "value is secret12-extended here"
	got := secpkg.RedactSecretValues(msg, secrets)
	// The longer value should be redacted first, preventing partial match.
	if strings.Contains(got, "secret12") {
		t.Errorf("partial secret leaked: %s", got)
	}
}
