package fusefile

import "testing"

func TestFusefileFieldsPresent(t *testing.T) {
	f := Fusefile{
		Version:   1,
		Image:     "img:tag",
		Resources: Resources{CPUs: 2, Memory: "2GB", Storage: "10GB", MaxRuntime: "1h", IdleTimeout: "15m"},
		Setup:     []Step{{Run: "echo hi"}},
		Services:  map[string]Service{"db": {Image: "postgres:16", Ports: []int{5432}, Env: map[string]EnvValue{"PGPASSWORD": {Secret: "pg_password"}, "PGUSER": {Value: "admin"}}}},
		Run:       "./start.sh",
		Workspace: "/workspace",
		Expose:    []Expose{{Port: 8080, As: "http"}},
		Secrets:   []string{"pg_password"},
	}

	// version field
	if f.Version != 1 {
		t.Errorf("version: got %d, want 1", f.Version)
	}

	// image field
	if f.Image != "img:tag" {
		t.Errorf("image: got %q, want %q", f.Image, "img:tag")
	}

	// resources fields
	if f.Resources.CPUs != 2 {
		t.Errorf("resources.cpus: got %d, want 2", f.Resources.CPUs)
	}
	if f.Resources.Memory != "2GB" {
		t.Errorf("resources.memory: got %q, want %q", f.Resources.Memory, "2GB")
	}
	if f.Resources.Storage != "10GB" {
		t.Errorf("resources.storage: got %q, want %q", f.Resources.Storage, "10GB")
	}
	if f.Resources.MaxRuntime != "1h" {
		t.Errorf("resources.max_runtime: got %q, want %q", f.Resources.MaxRuntime, "1h")
	}
	if f.Resources.IdleTimeout != "15m" {
		t.Errorf("resources.idle_timeout: got %q, want %q", f.Resources.IdleTimeout, "15m")
	}

	// setup field
	if len(f.Setup) != 1 || f.Setup[0].Run != "echo hi" {
		t.Errorf("setup: got %v, want [echo hi]", f.Setup)
	}

	// services field
	if len(f.Services) != 1 {
		t.Errorf("services: got %d services, want 1", len(f.Services))
	}
	dbService, ok := f.Services["db"]
	if !ok {
		t.Fatalf("services: key 'db' not found")
	}
	if dbService.Image != "postgres:16" {
		t.Errorf("services[db].image: got %q, want %q", dbService.Image, "postgres:16")
	}
	if len(dbService.Ports) != 1 || dbService.Ports[0] != 5432 {
		t.Errorf("services[db].ports: got %v, want [5432]", dbService.Ports)
	}

	// env field
	if len(dbService.Env) != 2 {
		t.Errorf("services[db].env: got %d entries, want 2", len(dbService.Env))
	}
	if pgpasswd, ok := dbService.Env["PGPASSWORD"]; !ok {
		t.Errorf("services[db].env: key PGPASSWORD not found")
	} else if pgpasswd.Secret != "pg_password" {
		t.Errorf("services[db].env[PGPASSWORD].secret: got %q, want %q", pgpasswd.Secret, "pg_password")
	}
	if pguser, ok := dbService.Env["PGUSER"]; !ok {
		t.Errorf("services[db].env: key PGUSER not found")
	} else if pguser.Value != "admin" {
		t.Errorf("services[db].env[PGUSER].value: got %q, want %q", pguser.Value, "admin")
	}

	// run field
	if f.Run != "./start.sh" {
		t.Errorf("run: got %q, want %q", f.Run, "./start.sh")
	}

	// workspace field
	if f.Workspace != "/workspace" {
		t.Errorf("workspace: got %q, want %q", f.Workspace, "/workspace")
	}

	// expose fields
	if len(f.Expose) != 1 {
		t.Errorf("expose: got %d entries, want 1", len(f.Expose))
	} else {
		if f.Expose[0].Port != 8080 {
			t.Errorf("expose[0].port: got %d, want 8080", f.Expose[0].Port)
		}
		if f.Expose[0].As != "http" {
			t.Errorf("expose[0].as: got %q, want %q", f.Expose[0].As, "http")
		}
	}

	// secrets field
	if len(f.Secrets) != 1 || f.Secrets[0] != "pg_password" {
		t.Errorf("secrets: got %v, want [pg_password]", f.Secrets)
	}
}
