package orchestrator

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/fusefile"
	"gopkg.in/yaml.v3"
)

func TestFuseComposePathConst(t *testing.T) {
	if fuseComposePath != "/fuse/compose.yaml" {
		t.Fatalf("fuseComposePath = %q, want /fuse/compose.yaml", fuseComposePath)
	}
}

func TestFusedAgentSpecWritesComposeWhenServicesDeclared(t *testing.T) {
	manifest := []byte(`{"version":"1","machine":{"workspace":"/workspace"},` +
		`"services":{"redis":{"image":"redis:7","ports":[6379],` +
		`"env":{"MODE":{"value":"prod"},"TOKEN":{"secret":"REDIS_TOKEN"}}}}}`)
	secretMap := map[string]string{"REDIS_TOKEN": "s3cr3t"}

	spec := FusedAgentSpec(manifest, secretMap, nil, BootOptions{})

	raw, ok := spec.Files[fuseComposePath]
	if !ok {
		t.Fatalf("expected a compose file at %s", fuseComposePath)
	}

	var proj struct {
		Services map[string]struct {
			Image       string            `yaml:"image"`
			Ports       []string          `yaml:"ports"`
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &proj); err != nil {
		t.Fatalf("compose is not valid yaml: %v", err)
	}
	svc, ok := proj.Services["redis"]
	if !ok {
		t.Fatalf("redis service missing from compose: %+v", proj)
	}
	if svc.Image != "redis:7" {
		t.Errorf("image = %q, want redis:7", svc.Image)
	}
	if len(svc.Ports) != 1 || svc.Ports[0] != "6379:6379" {
		t.Errorf("ports = %v, want [6379:6379]", svc.Ports)
	}
	if svc.Environment["MODE"] != "prod" {
		t.Errorf("env MODE = %q, want prod (literal value)", svc.Environment["MODE"])
	}
	if svc.Environment["TOKEN"] != "s3cr3t" {
		t.Errorf("env TOKEN = %q, want resolved secret s3cr3t", svc.Environment["TOKEN"])
	}
}

func TestFusedAgentSpecComposeRuntimeFields(t *testing.T) {
	manifest := []byte(`{"version":"1","machine":{"workspace":"/workspace"},"services":{` +
		`"api":{"image":"ghcr.io/acme/api:1","command":["/app/api","serve"],` +
		`"restart":"on-failure","depends_on":["db"],` +
		`"healthcheck":{"test":["CMD","curl","-fsS","http://localhost:8080/health"],` +
		`"interval":"10s","timeout":"2s","retries":3}},` +
		`"db":{"image":"postgres:16"}}}`)

	spec := FusedAgentSpec(manifest, nil, nil, BootOptions{})

	raw, ok := spec.Files[fuseComposePath]
	if !ok {
		t.Fatalf("expected a compose file at %s", fuseComposePath)
	}

	var proj struct {
		Services map[string]struct {
			Image       string   `yaml:"image"`
			Command     []string `yaml:"command"`
			Restart     string   `yaml:"restart"`
			DependsOn   []string `yaml:"depends_on"`
			HealthCheck *struct {
				Test     []string `yaml:"test"`
				Interval string   `yaml:"interval"`
				Timeout  string   `yaml:"timeout"`
				Retries  int      `yaml:"retries"`
			} `yaml:"healthcheck"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &proj); err != nil {
		t.Fatalf("compose is not valid yaml: %v", err)
	}

	svc, ok := proj.Services["api"]
	if !ok {
		t.Fatalf("api service missing from compose: %+v", proj)
	}
	if len(svc.Command) != 2 || svc.Command[0] != "/app/api" || svc.Command[1] != "serve" {
		t.Errorf("command = %v, want [/app/api serve]", svc.Command)
	}
	if svc.Restart != "on-failure" {
		t.Errorf("restart = %q, want on-failure", svc.Restart)
	}
	if len(svc.DependsOn) != 1 || svc.DependsOn[0] != "db" {
		t.Errorf("depends_on = %v, want [db]", svc.DependsOn)
	}
	if svc.HealthCheck == nil {
		t.Fatalf("healthcheck missing")
	}
	wantTest := []string{"CMD", "curl", "-fsS", "http://localhost:8080/health"}
	if len(svc.HealthCheck.Test) != len(wantTest) {
		t.Errorf("healthcheck.test = %v, want %v", svc.HealthCheck.Test, wantTest)
	}
	if svc.HealthCheck.Interval != "10s" || svc.HealthCheck.Timeout != "2s" || svc.HealthCheck.Retries != 3 {
		t.Errorf("healthcheck = %+v, want interval=10s timeout=2s retries=3", svc.HealthCheck)
	}

	// db declares none of the new fields; omitempty must keep them absent.
	db, ok := proj.Services["db"]
	if !ok {
		t.Fatalf("db service missing from compose: %+v", proj)
	}
	if db.Command != nil || db.Restart != "" || db.DependsOn != nil || db.HealthCheck != nil {
		t.Errorf("db service should have no compose runtime fields set, got %+v", db)
	}
}

func TestFusedAgentSpecNoComposeWhenNoServices(t *testing.T) {
	spec := FusedAgentSpec(DefaultFusedManifest, nil, nil, BootOptions{})
	if _, ok := spec.Files[fuseComposePath]; ok {
		t.Fatal("did not expect a compose file for a manifest with no services")
	}
}

func TestFuseEnvPathConst(t *testing.T) {
	if fuseEnvPath != "/fuse/env" {
		t.Fatalf("fuseEnvPath = %q, want /fuse/env", fuseEnvPath)
	}
}

func TestFusedAgentSpecWritesEnvFile(t *testing.T) {
	manifest := []byte(`{"version":"1","machine":{"workspace":"/workspace",` +
		`"env":{"NODE_ENV":{"value":"production"},"DATABASE_URL":{"secret":"db_url"},` +
		`"QUOTED":{"value":"it's here"}}},"services":{}}`)
	secretMap := map[string]string{"db_url": "postgres://u:p@h/db"}

	spec := FusedAgentSpec(manifest, secretMap, nil, BootOptions{})

	raw, ok := spec.Files[fuseEnvPath]
	if !ok {
		t.Fatalf("expected an env file at %s", fuseEnvPath)
	}

	// keys sorted, values single-quoted, one assignment per line.
	want := "DATABASE_URL='postgres://u:p@h/db'\nNODE_ENV='production'\nQUOTED='it'\\''s here'\n"
	if string(raw) != want {
		t.Fatalf("env file =\n%q\nwant\n%q", raw, want)
	}
}

func TestFusedAgentSpecNoEnvFileWhenNoEnv(t *testing.T) {
	spec := FusedAgentSpec(DefaultFusedManifest, nil, nil, BootOptions{})
	if _, ok := spec.Files[fuseEnvPath]; ok {
		t.Fatal("did not expect an env file for a manifest with no machine.env")
	}
}

// the manifest is caller-supplied on the raw create path, and a key cannot be
// quoted the way a value can, so a key that is not a shell identifier is
// dropped instead of becoming a statement the guest would source.
func TestEnvFileFromManifestDropsMalformedKeys(t *testing.T) {
	manifest := []byte(`{"machine":{"env":{"OK":{"value":"1"},` +
		`"BAD=x; rm -rf /":{"value":"2"},"2FAST":{"value":"3"},"":{"value":"4"}}}}`)

	raw, ok := envFileFromManifest(manifest, nil)
	if !ok {
		t.Fatal("expected an env file for the one well-formed key")
	}
	if string(raw) != "OK='1'\n" {
		t.Fatalf("env file = %q, want %q", raw, "OK='1'\n")
	}

	// nothing well-formed left means no file at all.
	if _, ok := envFileFromManifest([]byte(`{"machine":{"env":{"2FAST":{"value":"3"}}}}`), nil); ok {
		t.Fatal("did not expect an env file when every key is malformed")
	}
}

// end to end across the two packages that have to agree: a secret named in a
// Fusefile's top-level env block is resolved into /fuse/env at upload time and
// its value never appears in the script the orchestrator executes. The script
// reaches ssh as a single argv element, so its text is visible in the host's
// process table for the length of the boot.
func TestTopLevelEnvSecretStaysOutOfTheStartupScript(t *testing.T) {
	const secretValue = "postgres://user:hunter2@db.internal/app"

	f := &fusefile.Fusefile{Version: 1,
		Env:   map[string]fusefile.EnvValue{"DATABASE_URL": {Secret: "db_url"}},
		Setup: []fusefile.Step{{Run: "npm ci"}},
		Run:   fusefile.Command{Shell: "node server.js"}}
	c, err := fusefile.Compile(f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if want := []string{"db_url"}; !reflect.DeepEqual(c.RequiredSecrets, want) {
		t.Fatalf("required secrets = %v, want %v", c.RequiredSecrets, want)
	}

	if strings.Contains(c.StartupScript, secretValue) {
		t.Fatalf("the startup script carries the secret value:\n%s", c.StartupScript)
	}
	if !strings.Contains(c.StartupScript, ". "+fuseEnvPath) {
		t.Fatalf("the startup script does not source %s:\n%s", fuseEnvPath, c.StartupScript)
	}

	spec := FusedAgentSpec(c.ManifestJSON, map[string]string{"db_url": secretValue}, nil,
		BootOptions{StartupScript: c.StartupScript})

	raw, ok := spec.Files[fuseEnvPath]
	if !ok {
		t.Fatalf("expected an env file at %s", fuseEnvPath)
	}
	if !strings.Contains(string(raw), secretValue) {
		t.Fatalf("env file does not carry the resolved secret: %q", raw)
	}
	if strings.Contains(spec.Command, secretValue) {
		t.Fatalf("the fused command line carries the secret value: %s", spec.Command)
	}
}

func TestComposeFromManifestDeterministic(t *testing.T) {
	// unsorted service and env keys must still yield byte-identical output.
	manifest := []byte(`{"services":{"b":{"image":"img-b",` +
		`"env":{"Z":{"value":"1"},"A":{"value":"2"}}},"a":{"image":"img-a"}}}`)

	out1, ok1 := composeFromManifest(manifest, nil)
	out2, ok2 := composeFromManifest(manifest, nil)
	if !ok1 || !ok2 {
		t.Fatal("expected services to be present")
	}
	if string(out1) != string(out2) {
		t.Errorf("composeFromManifest not deterministic:\n%s\n---\n%s", out1, out2)
	}
}

// The probe reaches the guest as a file and nothing else: the firecracker host
// agent ignores AgentSpec.Command, so Files is the only channel that gets
// there.
func TestFusedAgentSpecWritesHealthcheckFile(t *testing.T) {
	probe := &HealthcheckSpec{
		HTTP:               &HealthcheckHTTP{Port: 8080, Path: "/healthz"},
		IntervalSeconds:    5,
		TimeoutSeconds:     2,
		Retries:            12,
		StartPeriodSeconds: 10,
	}
	spec := FusedAgentSpec(DefaultFusedManifest, nil, nil, BootOptions{Healthcheck: probe})

	raw, ok := spec.Files[GuestHealthcheckPath]
	if !ok {
		t.Fatalf("expected a healthcheck file at %s", GuestHealthcheckPath)
	}
	var got HealthcheckSpec
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("healthcheck file is not valid json: %v", err)
	}
	if got.HTTP == nil || got.HTTP.Port != 8080 || got.HTTP.Path != "/healthz" {
		t.Errorf("http = %+v, want port 8080 path /healthz", got.HTTP)
	}
	if got.IntervalSeconds != 5 || got.TimeoutSeconds != 2 || got.Retries != 12 || got.StartPeriodSeconds != 10 {
		t.Errorf("timings = %+v, want 5/2/12/10", got)
	}
}

// No probe means no file: its absence is what tells the guest agent not to
// probe at all, so writing an empty one would start a probe nobody asked for.
func TestFusedAgentSpecNoHealthcheckFileWhenUnset(t *testing.T) {
	spec := FusedAgentSpec(DefaultFusedManifest, nil, nil, BootOptions{})
	if _, ok := spec.Files[GuestHealthcheckPath]; ok {
		t.Errorf("wrote %s for an environment with no healthcheck", GuestHealthcheckPath)
	}
}
