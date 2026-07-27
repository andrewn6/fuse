// Command orchestrator runs the Fuse orchestrator REST API.
//
// It boots a FleetManager (currently backed by the Firecracker provider,
// with an in-memory stub fallback when FIRECRACKER_BASE_URL is unset),
// mounts the api package's chi router, and serves HTTP with graceful
// shutdown on SIGINT / SIGTERM.
//
// Configuration is flag-driven, with env-var fallbacks for anything
// that's reasonable to set in an operator environment.
//
//	@title						Fuse Orchestrator API
//	@version					0.1.0
//	@description				Control plane for Fuse orchestrator. Provisions, inspects, and destroys VMs; manages snapshots.
//	@host						localhost:8080
//	@BasePath					/
//	@securityDefinitions.apikey	BearerAuth
//	@in							header
//	@name						Authorization
package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/folsomintel/fuse/internal/api"
	"github.com/folsomintel/fuse/internal/apikeys"
	"github.com/folsomintel/fuse/internal/firecracker"
	"github.com/folsomintel/fuse/internal/metrics"
	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/folsomintel/fuse/internal/qemu"
)

// version is stamped at release time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "orchestrator: %v\n", err)
		os.Exit(1)
	}
}

// env returns os.Getenv(name) or fallback when unset.
func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// apiKeyStoreOrNil returns s as an api.APIKeyStore, or an untyped nil when
// s is nil. Assigning a typed-nil *apikeys.APIKeyStore directly to the
// interface field would make it compare non-nil and enable key auth with a
// nil store; this keeps the "no DB ⇒ no key auth" contract correct.
func apiKeyStoreOrNil(s *apikeys.APIKeyStore) api.APIKeyStore {
	if s == nil {
		return nil
	}
	return s
}

// envInt parses an int env var, returning fallback on miss or parse error.
func envInt(name string, fallback int) int {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// resolveTLS reports whether the orchestrator terminates TLS itself. Exactly
// one of cert/key is always a misconfiguration: the old behaviour silently
// fell back to plaintext HTTP, which is the worst outcome for an operator who
// believed they had enabled TLS.
func resolveTLS(tlsCert, tlsKey string) (bool, error) {
	switch {
	case tlsCert != "" && tlsKey != "":
		return true, nil
	case tlsCert != "":
		return false, errors.New("ORCH_TLS_CERT is set but ORCH_TLS_KEY is not; set both to serve TLS, or neither to serve plaintext HTTP")
	case tlsKey != "":
		return false, errors.New("ORCH_TLS_KEY is set but ORCH_TLS_CERT is not; set both to serve TLS, or neither to serve plaintext HTTP")
	default:
		return false, nil
	}
}

// resolveSecureCookies decides whether session cookies get the Secure flag.
//
// The session cookie carries the master token, so "is this connection
// encrypted?" has to be answered by configuration, not by the request. In the
// documented reverse-proxy topology the orchestrator itself speaks plaintext
// HTTP and only the proxy sees TLS, so deriving the flag from the listener
// alone drops Secure on exactly the deployments that most need it. Forwarded
// headers are deliberately not consulted: any client can send
// X-Forwarded-Proto, so trusting it would let a caller talk the orchestrator
// into either setting or dropping the flag.
//
// setting is the raw ORCH_SECURE_COOKIES value: empty derives from useTLS.
func resolveSecureCookies(setting string, useTLS bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(setting)) {
	case "":
		return useTLS, nil
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	default:
		return false, fmt.Errorf("ORCH_SECURE_COOKIES: want true, false, or empty; got %q", setting)
	}
}

func run() error {
	var (
		listenAddr        string
		prefix            string
		readHeaderTimeout time.Duration
		writeTimeout      time.Duration
		idleTimeout       time.Duration
		maxHeaderBytes    int
		shutdownTimeout   time.Duration
		fcBaseURL         string
		fcToken           string
		databaseURL       string
		tlsCert           string
		tlsKey            string
		authToken         string
		allowedCIDRs      string
		secureCookies     string
	)

	flag.StringVar(&listenAddr, "listen", env("ORCH_LISTEN", ":8080"),
		"HTTP listen address")
	flag.StringVar(&prefix, "vm-prefix", env("ORCH_VM_PREFIX", "fuse-"),
		"VM name prefix used by the fleet manager")
	flag.DurationVar(&readHeaderTimeout, "read-header-timeout", 5*time.Second,
		"max time to read request headers")
	flag.DurationVar(&writeTimeout, "write-timeout", 60*time.Second,
		"max time to write a response (including streaming handlers)")
	flag.DurationVar(&idleTimeout, "idle-timeout", 120*time.Second,
		"max time an idle keep-alive connection may stay open")
	flag.IntVar(&maxHeaderBytes, "max-header-bytes", 64<<10,
		"max size of request headers")
	flag.DurationVar(&shutdownTimeout, "shutdown-timeout",
		time.Duration(envInt("ORCH_SHUTDOWN_TIMEOUT_SECONDS", 30))*time.Second,
		"graceful shutdown ceiling")
	flag.StringVar(&fcBaseURL, "firecracker-url", env("FIRECRACKER_BASE_URL", ""),
		"Firecracker host-agent base URL (empty = in-memory stub)")
	flag.StringVar(&fcToken, "firecracker-token", env("FIRECRACKER_TOKEN", ""),
		"Firecracker host-agent auth token")
	flag.StringVar(&databaseURL, "database-url", env("DATABASE_URL", ""),
		"Postgres connection string (empty = in-memory state store)")
	flag.StringVar(&tlsCert, "tls-cert", env("ORCH_TLS_CERT", ""),
		"path to TLS certificate PEM (empty = plaintext HTTP)")
	flag.StringVar(&tlsKey, "tls-key", env("ORCH_TLS_KEY", ""),
		"path to TLS private key PEM")
	flag.StringVar(&authToken, "auth-token", env("ORCH_AUTH_TOKEN", ""),
		"static Bearer token for API auth (empty = no auth)")
	flag.StringVar(&allowedCIDRs, "allowed-cidrs", env("ORCH_ALLOWED_CIDRS", ""),
		"comma-separated CIDR allowlist (empty = open access)")
	flag.StringVar(&secureCookies, "secure-cookies", env("ORCH_SECURE_COOKIES", ""),
		`force the Secure flag on session cookies: "true" behind a TLS-terminating `+
			`proxy, "false" for plaintext local development, empty = derive from -tls-cert/-tls-key`)

	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print version and exit")

	tokenEncKeyHex := env("TOKEN_ENCRYPTION_KEY", "")

	// Strict mode: refuse to boot when running unauthenticated or
	// without persisted-token encryption. Set ORCH_REQUIRE_AUTH=true
	// in any production-like deploy (e.g. Railway). The flag is
	// opt-in so dev/test can keep running with empty values.
	requireAuth := env("ORCH_REQUIRE_AUTH", "") == "true"

	flag.Parse()

	if showVersion {
		fmt.Println(version)
		return nil
	}

	// Parse token encryption key (hex-encoded 32 bytes = 64 hex chars).
	var tokenEncKey []byte
	if tokenEncKeyHex != "" {
		var err error
		tokenEncKey, err = hex.DecodeString(tokenEncKeyHex)
		if err != nil {
			return fmt.Errorf("TOKEN_ENCRYPTION_KEY: invalid hex: %w", err)
		}
		if len(tokenEncKey) != 32 {
			return fmt.Errorf("TOKEN_ENCRYPTION_KEY: must be 32 bytes (64 hex chars), got %d bytes", len(tokenEncKey))
		}
	}

	// Strict-mode enforcement. Done after parsing so error messages
	// can surface every missing prereq at once rather than one at a
	// time on successive restarts.
	if requireAuth {
		var missing []string
		if authToken == "" {
			missing = append(missing, "ORCH_AUTH_TOKEN")
		}
		if len(tokenEncKey) == 0 {
			missing = append(missing, "TOKEN_ENCRYPTION_KEY")
		}
		if databaseURL == "" {
			missing = append(missing, "DATABASE_URL")
		}
		if len(missing) > 0 {
			return fmt.Errorf("ORCH_REQUIRE_AUTH=true but required vars unset: %s", strings.Join(missing, ", "))
		}
	}

	// TLS and cookie policy, resolved before anything binds a socket so a
	// half-configured deployment fails at startup rather than serving
	// plaintext with a sensitive cookie on it.
	useTLS, err := resolveTLS(tlsCert, tlsKey)
	if err != nil {
		return err
	}
	useSecureCookies, err := resolveSecureCookies(secureCookies, useTLS)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if !useTLS && useSecureCookies {
		logger.Info("serving plaintext HTTP with Secure session cookies; " +
			"this is only correct behind a TLS-terminating proxy")
	}
	if !useTLS && !useSecureCookies {
		logger.Warn("session cookies are not marked Secure; " +
			"set ORCH_SECURE_COOKIES=true when a proxy terminates TLS in front of this listener")
	}

	// Firecracker is the only backend. Empty FIRECRACKER_BASE_URL falls back
	// to an in-memory stub inside the firecracker package — that's the dev
	// default. AGENT_DOWNLOAD_URL (optional) fetches the guest agent binary
	// into the VM at boot.
	provider := firecracker.New(firecracker.Config{
		BaseURL:     fcBaseURL,
		Token:       fcToken,
		DownloadURL: env("AGENT_DOWNLOAD_URL", ""),
	})
	mode := "firecracker"
	if fcBaseURL == "" {
		mode = "firecracker-stub"
	}

	// State store: Postgres if DATABASE_URL is set, in-memory otherwise.
	var store orchestrator.StateStore
	var apiKeyStore *apikeys.APIKeyStore
	if databaseURL != "" {
		db, err := sql.Open("pgx", databaseURL)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = db.Close() }()

		// Verify connectivity.
		if err := db.PingContext(context.Background()); err != nil {
			return fmt.Errorf("ping database: %w", err)
		}

		pgStore := orchestrator.NewPostgresStateStore(db)
		if err := pgStore.ApplyMigrations(context.Background()); err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}
		store = pgStore
		// API keys share the same DB and migrations. Without a database
		// there is nowhere to persist keys, so key auth is Postgres-only;
		// the master token still works in either case.
		apiKeyStore = apikeys.NewAPIKeyStore(db)
		logger.Info("state store: postgres", "url", redactDSN(databaseURL))
	} else {
		store = orchestrator.NewMemoryStateStore()
		logger.Info("state store: in-memory (no DATABASE_URL)")
	}

	promMetrics := metrics.NewPrometheusMetrics(prometheus.DefaultRegisterer)

	// Single shared factory for per-host providers, routed by the host's
	// virtualization backend. Used both at the API layer (POST /v1/hosts)
	// and during recovery to rehydrate hosts loaded from the state store
	// after a restart.
	//
	// The qemu backend routes to the qemu provider (gpu passthrough hosts);
	// every other backend uses firecracker. An empty host url makes either
	// provider fall back to its in-memory stub, matching the dev/test path.
	hostProviderFactory := func(url, token string, backend orchestrator.HostBackend) orchestrator.Provider {
		if backend == orchestrator.BackendQEMU {
			return qemu.New(qemu.Config{
				BaseURL:     url,
				Token:       token,
				DownloadURL: env("AGENT_DOWNLOAD_URL", ""),
			})
		}
		return firecracker.New(firecracker.Config{
			BaseURL:     url,
			Token:       token,
			DownloadURL: env("AGENT_DOWNLOAD_URL", ""),
		})
	}

	fm := orchestrator.NewFleetManager(orchestrator.FleetConfig{
		Provider:            provider,
		StateStore:          store,
		Prefix:              prefix,
		TokenEncryptionKey:  tokenEncKey,
		HostProviderFactory: hostProviderFactory,
		Metrics:             promMetrics,
		Logger:              logger,
	})

	// Reconcile loop starts with the binary.
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	fm.Start(rootCtx)
	defer fm.Stop()

	// Parse CIDR allowlist.
	var cidrList []string
	if allowedCIDRs != "" {
		for _, c := range strings.Split(allowedCIDRs, ",") {
			c = strings.TrimSpace(c)
			if c != "" {
				cidrList = append(cidrList, c)
			}
		}
	}

	// Audit callbacks for auth/IP rejections. The requestID
	// argument comes from api.RequestIDMiddleware and lets ops
	// correlate the audit event with the matching response (whose
	// X-Request-ID header carries the same value).
	auditAuthFail := func(remoteAddr, method, path, requestID string) {
		fm.AuditEvent(context.Background(), "api", remoteAddr, "auth.failed", map[string]any{
			"method":     method,
			"path":       path,
			"request_id": requestID,
		})
		logger.Warn("auth failed",
			"remote", remoteAddr,
			"method", method,
			"path", path,
			"request_id", requestID,
		)
	}
	auditIPReject := func(remoteAddr, method, path, requestID string) {
		fm.AuditEvent(context.Background(), "api", remoteAddr, "ip.rejected", map[string]any{
			"method":     method,
			"path":       path,
			"request_id": requestID,
		})
		logger.Warn("ip rejected",
			"remote", remoteAddr,
			"method", method,
			"path", path,
			"request_id", requestID,
		)
	}

	handler := &api.Handler{
		Fleet:                   fm,
		NewProvider:             hostProviderFactory,
		AuthToken:               authToken,
		APIKeys:                 apiKeyStoreOrNil(apiKeyStore),
		AllowedCIDRs:            cidrList,
		SecureCookies:           useSecureCookies,
		OnAuthFailure:           auditAuthFail,
		OnIPReject:              auditIPReject,
		Version:                 version,
		MetricsRequestsTotal:    promMetrics.HTTPRequestsTotal,
		MetricsRequestDuration:  promMetrics.HTTPRequestDuration,
		MetricsRequestsInFlight: promMetrics.HTTPRequestsInFlight,
	}

	router, err := handler.Router()
	if err != nil {
		return fmt.Errorf("build router: %w", err)
	}

	// Serve /metrics, /health, and /ready outside the auth-protected
	// router so Prometheus and load-balancer / k8s probes can scrape
	// without a Bearer token. These probes intentionally do not (and
	// shouldn't) carry credentials.
	hc := &api.Healthcheck{Fleet: fm, Store: store, BuildVersion: version}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health", hc.Liveness)
	mux.HandleFunc("/ready", hc.Readiness)
	mux.HandleFunc("/v1/version", hc.Version)
	mux.Handle("/", router)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		// Bound how long an idle keep-alive connection may sit doing nothing,
		// so a client cannot pin connections open indefinitely without ever
		// sending a request. Note there is deliberately no ReadTimeout: it
		// would arm a read deadline for the whole request, and Go reads its
		// expiry as a dead client, which would cut off the SSE event stream
		// and attach sessions that hold a connection open on purpose. Body
		// reads are bounded per request in internal/api instead.
		IdleTimeout: idleTimeout,
		// Cap request headers well below Go's 1 MiB default. Nothing this API
		// accepts needs more than a bearer token and a handful of standard
		// headers.
		MaxHeaderBytes: maxHeaderBytes,
	}

	// Signal handling + graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	scheme := "http"
	if useTLS {
		scheme = "https"
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("orchestrator server listening",
			"addr", listenAddr,
			"scheme", scheme,
			"provider", mode,
			"prefix", prefix,
			"auth", authToken != "",
			"cidrs", len(cidrList),
		)
		if useTLS {
			if err := srv.ListenAndServeTLS(tlsCert, tlsKey); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		} else {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		}
	}()

	select {
	case sig := <-sigCh:
		logger.Info("shutdown signal received", "signal", sig.String())
	case err := <-errCh:
		return fmt.Errorf("http listener: %w", err)
	}

	// Graceful shutdown bounded by shutdownTimeout.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	logger.Info("orchestrator server stopped")
	return nil
}

// redactDSN masks the password in a Postgres connection string for
// safe logging. If parsing fails it falls back to "<unparseable>".
func redactDSN(dsn string) string {
	// Quick-and-dirty: replace anything between ":" and "@" after "://"
	// with "***". Handles both postgres://user:pass@host and
	// postgres://user@host (no password).
	const prefix = "://"
	idx := 0
	for i := range dsn {
		if i+3 <= len(dsn) && dsn[i:i+3] == prefix {
			idx = i + 3
			break
		}
	}
	if idx == 0 {
		return "<unparseable>"
	}
	atIdx := -1
	for i := idx; i < len(dsn); i++ {
		if dsn[i] == '@' {
			atIdx = i
			break
		}
	}
	if atIdx < 0 {
		return dsn // no @ — no credentials to redact
	}
	colonIdx := -1
	for i := idx; i < atIdx; i++ {
		if dsn[i] == ':' {
			colonIdx = i
			break
		}
	}
	if colonIdx < 0 {
		return dsn // user but no password
	}
	return dsn[:colonIdx+1] + "***" + dsn[atIdx:]
}
