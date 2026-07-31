package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandler_HealthIsUnauthenticated(t *testing.T) {
	srv := httptest.NewServer(newHandler(config{vmID: "fuse-1"}, "secret-token", []byte("{}"), 0, false))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/health = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" || body["vm_id"] != "fuse-1" {
		t.Fatalf("/health body = %+v", body)
	}
}

func TestHandler_InfoRequiresToken(t *testing.T) {
	manifest := []byte(`{"version":"1"}`)
	srv := httptest.NewServer(newHandler(config{vmID: "fuse-1"}, "secret-token", manifest, 3, false))
	defer srv.Close()

	// No token -> 401.
	resp, err := http.Get(srv.URL + "/v1/info")
	if err != nil {
		t.Fatalf("GET /v1/info: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/v1/info (no token) = %d, want 401", resp.StatusCode)
	}

	// Correct token -> 200 with the booted-with details.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/info", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/info (auth): %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("/v1/info (auth) = %d, want 200", resp2.StatusCode)
	}
	var info map[string]any
	json.NewDecoder(resp2.Body).Decode(&info)
	if info["secret_count"].(float64) != 3 || info["manifest_bytes"].(float64) != float64(len(manifest)) {
		t.Fatalf("/v1/info body = %+v", info)
	}
}

// TestHandler_InfoOpenWhenNoToken verifies that with no auth token configured
// (dev/insecure), /v1/info is reachable without a header.
func TestHandler_InfoOpenWhenNoToken(t *testing.T) {
	srv := httptest.NewServer(newHandler(config{vmID: "fuse-1"}, "", nil, 0, false))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/info")
	if err != nil {
		t.Fatalf("GET /v1/info: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/info (no auth configured) = %d, want 200", resp.StatusCode)
	}
}
