// Command fused is the reference in-guest agent for Fuse.
//
// It runs inside each Firecracker microVM, launched by the host fc-agent
// (host-agent/fc-agent.py) via a systemd unit. Fuse uploads the manifest, secrets,
// and (optionally) TLS + auth-token files to /fuse/* before starting it; fused
// reads them and serves a small HTTP API on --listen (default 0.0.0.0:9550),
// which the control plane reaches through the host's per-VM DNAT.
//
// This is intentionally minimal — a reference implementation of the guest-agent
// contract Fuse expects (a long-lived process that binds --listen and quiesces
// on SIGTERM). Bring your own agent by implementing the same flags + a /health
// endpoint and baking it in as /usr/local/bin/fused (see host-agent/).
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// version is stamped at release time via -ldflags "-X main.version=...".
var version = "dev"

// config holds the flags fc-agent passes on the ExecStart line.
type config struct {
	listen        string
	manifestPath  string
	secretsPath   string
	authTokenFile string
	tlsCert       string
	tlsKey        string
	gateway       string
	gatewayToken  string
	vmID          string
	insecure      bool
	showVersion   bool

	// healthcheck is the environment-level probe's config, and healthState is
	// where its verdict is written for the orchestrator to read back. Both
	// default to the paths the orchestrator uploads to, because the
	// firecracker host agent builds its own ExecStart line and would never
	// pass a flag added after the wire was frozen. See health.go.
	healthcheck string
	healthState string

	// display is the X display the computer routes drive. See computer.go.
	display string

	// desktop is the declared desktop geometry's path, uploaded by the
	// orchestrator like the healthcheck config. See desktop.go.
	desktop string
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.listen, "listen", "0.0.0.0:9550", "address to bind the agent HTTP API")
	flag.StringVar(&c.manifestPath, "manifest", "", "path to the uploaded manifest JSON")
	flag.StringVar(&c.secretsPath, "secrets", "", "path to the uploaded secrets JSON")
	flag.StringVar(&c.authTokenFile, "auth-token-file", "", "path to the bearer auth token file")
	flag.StringVar(&c.tlsCert, "tls-cert", "", "path to the TLS certificate PEM")
	flag.StringVar(&c.tlsKey, "tls-key", "", "path to the TLS private key PEM")
	flag.StringVar(&c.gateway, "gateway", "", "gateway websocket URL (pass-through)")
	flag.StringVar(&c.gatewayToken, "gateway-token", "", "gateway token (pass-through)")
	flag.StringVar(&c.vmID, "vm-id", "", "VM identifier assigned by the orchestrator")
	flag.StringVar(&c.healthcheck, "healthcheck", "/fuse/healthcheck.json", "path to the environment healthcheck config (absent means no probe)")
	flag.StringVar(&c.healthState, "health-state", "/fuse/health.json", "path to write the healthcheck verdict for the orchestrator to read")
	flag.StringVar(&c.display, "display", ":1", "X display the computer routes drive (desktop images only)")
	flag.StringVar(&c.desktop, "desktop", "/fuse/desktop.json", "path to the declared desktop geometry (absent means the image default)")
	flag.BoolVar(&c.insecure, "insecure", false, "run without TLS/auth (dev only)")
	flag.BoolVar(&c.showVersion, "version", false, "print version and exit")
	flag.Parse()
	return c
}

func main() {
	c := parseFlags()
	if c.showVersion {
		fmt.Println(version)
		return
	}
	log.SetPrefix("[fused] ")
	log.SetFlags(log.LstdFlags | log.LUTC)

	// Best-effort load of the manifest/secrets so /v1/info can report them and
	// startup fails loudly if a declared path is unreadable.
	manifestBytes := readIfSet(c.manifestPath, "manifest")
	secretCount := countSecrets(c.secretsPath)

	var authToken string
	if c.authTokenFile != "" {
		b, err := os.ReadFile(c.authTokenFile)
		if err != nil {
			log.Fatalf("read auth token file %s: %v", c.authTokenFile, err)
		}
		authToken = strings.TrimSpace(string(b))
	}

	// Decide TLS. The Fuse host agent always passes --tls-cert/--tls-key on the
	// frozen wire, but the cert/key files are only uploaded when the orchestrator
	// has a TOKEN_ENCRYPTION_KEY (per-VM creds). So enable TLS only when the
	// files are actually present and non-empty; otherwise fall back to plaintext
	// rather than crashing on a path that points at nothing.
	useTLS := c.tlsCert != "" && c.tlsKey != ""
	if useTLS && (!fileNonEmpty(c.tlsCert) || !fileNonEmpty(c.tlsKey)) {
		log.Printf("WARNING: --tls-cert/--tls-key given but files missing/empty (%s, %s); serving plaintext", c.tlsCert, c.tlsKey)
		useTLS = false
	}

	// Load the environment-level probe. A missing config file is the ordinary
	// "no healthcheck declared" case and yields a nil prober; a config that
	// exists but is unusable is fatal, because it only reached the guest
	// because somebody asked for a probe and silently running none would leave
	// them believing the environment was being checked.
	probe, err := loadProber(c.healthcheck, c.healthState)
	if err != nil {
		log.Fatalf("%v", err)
	}

	srv := &http.Server{
		Addr:              c.listen,
		Handler:           newHandler(c, authToken, manifestBytes, secretCount, useTLS, probe),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Graceful shutdown: systemd `stop` (the Fuse drain command) delivers
	// SIGTERM; quiesce and exit 0 so Drain records a clean stop.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The probe loop shares the shutdown context, so SIGTERM stops it along
	// with the server. A nil prober returns immediately.
	go probe.run(ctx)

	// Reconcile the display with the declared desktop geometry, off the
	// serving path: a restarting display answers 503 on the computer routes
	// until it is back, which is the contract those routes already have.
	go applyDesktop(c.desktop, c.display)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on %s (vm=%s tls=%t auth=%t manifest=%dB secrets=%d gateway=%t)",
			c.listen, c.vmID, useTLS, authToken != "", len(manifestBytes), secretCount, c.gateway != "")
		if useTLS {
			errCh <- srv.ListenAndServeTLS(c.tlsCert, c.tlsKey)
		} else {
			errCh <- srv.ListenAndServe()
		}
	}()

	select {
	case <-ctx.Done():
		log.Print("shutdown signal received, draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Fatalf("graceful shutdown failed: %v", err)
		}
		log.Print("stopped cleanly")
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}
}

// newHandler builds the agent's HTTP routes. /health is unauthenticated so the
// host and load balancers can probe it; /v1/info is bearer-protected when a
// token was provided. Extracted from main so it can be exercised in tests.
//
// The environment-level probe's verdict rides along on /health as a nested
// object, absent when no probe was declared. The status code deliberately
// stays 200 whatever the verdict says: /health answers "is the agent up",
// which is a different question from "is the workload healthy", and anything
// already treating a non-200 here as a dead agent must keep working. Callers
// that want the verdict read the field.
func newHandler(c config, authToken string, manifestBytes []byte, secretCount int, useTLS bool, probe *prober) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{"status": "ok", "vm_id": c.vmID}
		if probe != nil {
			body["healthcheck"] = probe.snapshot()
		}
		writeJSON(w, http.StatusOK, body)
	})
	mux.HandleFunc("/v1/info", protect(authToken, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"vm_id":          c.vmID,
			"manifest_bytes": len(manifestBytes),
			"secret_count":   secretCount,
			"gateway":        c.gateway != "",
			"tls":            useTLS,
		})
	}))

	// The computer control surface. Bearer-protected like /v1/info; on an
	// image with no display the routes answer 503 with a reason, so
	// registering them unconditionally is safe. See computer.go.
	comp := newComputer(c.display)
	mux.HandleFunc("/v1/computer/action", protect(authToken, comp.handleAction))
	mux.HandleFunc("/v1/computer/screenshot", protect(authToken, comp.handleScreenshot))
	mux.HandleFunc("/v1/computer/display", protect(authToken, comp.handleDisplay))
	mux.HandleFunc("/v1/computer/stream", protect(authToken, comp.handleStream))
	return mux
}

// fileNonEmpty reports whether path exists and has non-zero size.
func fileNonEmpty(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}

// protect wraps h with a constant-time bearer check when token is non-empty.
func protect(token string, h http.HandlerFunc) http.HandlerFunc {
	if token == "" {
		return h
	}
	want := "Bearer " + token
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func readIfSet(path, label string) []byte {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s %s: %v", label, path, err)
	}
	return b
}

// countSecrets parses the secrets JSON (a flat string map) and returns its size,
// or 0 when unset/empty. A malformed file is fatal — it signals a bad upload.
func countSecrets(path string) int {
	b := readIfSet(path, "secrets")
	if len(b) == 0 {
		return 0
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		log.Fatalf("parse secrets %s: %v", path, err)
	}
	return len(m)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
