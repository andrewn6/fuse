package main

import "testing"

// TestResolveTLS covers the four cert/key combinations. The partial ones must
// fail rather than quietly downgrade to plaintext.
func TestResolveTLS(t *testing.T) {
	tests := []struct {
		name    string
		cert    string
		key     string
		want    bool
		wantErr bool
	}{
		{name: "both set", cert: "cert.pem", key: "key.pem", want: true},
		{name: "neither set"},
		{name: "cert only", cert: "cert.pem", wantErr: true},
		{name: "key only", key: "key.pem", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveTLS(tt.cert, tt.key)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveTLS(%q, %q): want error, got nil", tt.cert, tt.key)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTLS(%q, %q): %v", tt.cert, tt.key, err)
			}
			if got != tt.want {
				t.Fatalf("resolveTLS(%q, %q) = %v, want %v", tt.cert, tt.key, got, tt.want)
			}
		})
	}
}

// TestResolveSecureCookies covers the deployment topologies from the security
// docs: direct TLS, TLS terminated at a proxy, and explicit plaintext local
// development.
func TestResolveSecureCookies(t *testing.T) {
	tests := []struct {
		name    string
		setting string
		useTLS  bool
		want    bool
		wantErr bool
	}{
		{name: "direct TLS defaults to secure", useTLS: true, want: true},
		{name: "plaintext defaults to insecure"},
		{name: "proxy TLS forces secure", setting: "true", want: true},
		{name: "proxy TLS forces secure uppercase", setting: "TRUE", want: true},
		{name: "local dev opts out explicitly", setting: "false"},
		{name: "explicit opt-out wins over direct TLS", setting: "false", useTLS: true},
		{name: "explicit opt-in is a no-op under direct TLS", setting: "true", useTLS: true, want: true},
		{name: "garbage is rejected", setting: "maybe", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveSecureCookies(tt.setting, tt.useTLS)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveSecureCookies(%q, %v): want error, got nil", tt.setting, tt.useTLS)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSecureCookies(%q, %v): %v", tt.setting, tt.useTLS, err)
			}
			if got != tt.want {
				t.Fatalf("resolveSecureCookies(%q, %v) = %v, want %v", tt.setting, tt.useTLS, got, tt.want)
			}
		})
	}
}
