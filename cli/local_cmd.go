package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/folsomintel/fuse/cli/config"
	hostagent "github.com/folsomintel/fuse/host-agent"
	fuse "github.com/folsomintel/fuse/sdks/go"
)

// fuse local runs the real fuse stack (orchestrator + firecracker host agent)
// on this machine for development. on linux it runs directly against
// /dev/kvm; on macOS it boots a small linux appliance VM with vfkit (nested
// virtualization, so firecracker inside is real) and runs the same stack in
// there. either way the cli ends up with a context named "local" pointing at
// a working orchestrator with one registered host, and `fuse up` works
// against it exactly as it would against a fleet.
const (
	localContextName = "local"
	localHostID      = "local"
	localOrchPort    = 8080
	localAgentPort   = 8090
	// localMaxVMs is the registered vm_count. it is scheduling policy, not a
	// hardware fact, so it must be declared; a laptop-friendly default.
	localMaxVMs = 8
)

func newLocalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "local",
		Short: "Run a local fuse stack for development",
		Long: "local runs the real fuse stack on this machine: the orchestrator plus the\n" +
			"firecracker host agent, booting real microVMs.\n\n" +
			"on linux it needs /dev/kvm, python3, and sudo. on macOS it needs vfkit\n" +
			"(brew install vfkit) and an M3 or later on macOS 15+, because firecracker\n" +
			"runs inside a linux appliance VM under nested virtualization.\n\n" +
			"`fuse local up` leaves the cli connected to a context named \"local\", so\n" +
			"`fuse up` in any project just works. environments run arm64 images on\n" +
			"apple silicon and your machine's native arch on linux; the scheduler's\n" +
			"arch field keeps mixed fleets honest.",
	}
	cmd.AddCommand(newLocalUpCmd(), newLocalDownCmd(), newLocalStatusCmd())
	return cmd
}

// localState is everything fuse local persists between invocations, all under
// one directory (~/.fuse/local).
type localState struct {
	dir             string
	fcAgentToken    string
	orchAuthToken   string
	setupScript     string // path to the written fuse-local-setup.sh
	fcAgentScript   string // path to the written fc-agent.py
	sshKey          string // path to the appliance ssh private key (darwin)
	applianceDisk   string // path to the appliance raw disk (darwin)
	vfkitPid        string // path to the vfkit pidfile (darwin)
	applianceSerial string // path to the appliance console log (darwin)
}

func localStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	return filepath.Join(home, ".fuse", "local"), nil
}

// loadLocalState creates the state dir, writes the embedded assets, and loads
// (or mints) the two stack tokens. it is idempotent: tokens persist across
// invocations so a re-up talks to the same stack.
func loadLocalState() (*localState, error) {
	dir, err := localStateDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &localState{
		dir:             dir,
		setupScript:     filepath.Join(dir, "fuse-local-setup.sh"),
		fcAgentScript:   filepath.Join(dir, "fc-agent.py"),
		sshKey:          filepath.Join(dir, "appliance_ed25519"),
		applianceDisk:   filepath.Join(dir, "appliance.raw"),
		vfkitPid:        filepath.Join(dir, "vfkit.pid"),
		applianceSerial: filepath.Join(dir, "appliance-console.log"),
	}
	// the embedded assets are rewritten every time so a cli upgrade upgrades
	// the stack scripts with it.
	if err := os.WriteFile(s.setupScript, hostagent.LocalSetupSh, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(s.fcAgentScript, hostagent.FCAgentPy, 0o644); err != nil {
		return nil, err
	}
	if s.fcAgentToken, err = loadOrMintToken(filepath.Join(dir, "fc-agent.token")); err != nil {
		return nil, err
	}
	if s.orchAuthToken, err = loadOrMintToken(filepath.Join(dir, "orchestrator.token")); err != nil {
		return nil, err
	}
	return s, nil
}

func loadOrMintToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// localReleaseVersion pins the stack's release-asset downloads (orchestrator,
// fused) to the cli's own version, so the local stack matches the cli. a dev
// build ("dev") falls back to the latest release.
func localReleaseVersion() string {
	if version == "dev" {
		return "latest"
	}
	return "v" + strings.TrimPrefix(version, "v")
}

func newLocalUpCmd() *cobra.Command {
	var (
		cpus   int
		memMB  int
		diskGB int
	)
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start the local stack and connect the CLI to it",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := loadLocalState()
			if err != nil {
				return err
			}
			switch runtime.GOOS {
			case "linux":
				return localUpLinux(cmd.Context(), s)
			case "darwin":
				return localUpDarwin(cmd.Context(), s, cpus, memMB, diskGB)
			default:
				return fmt.Errorf("fuse local supports linux and macOS, not %s", runtime.GOOS)
			}
		},
	}
	cmd.Flags().IntVar(&cpus, "cpus", 4, "appliance vcpus (macOS only)")
	cmd.Flags().IntVar(&memMB, "memory", 6144, "appliance ram in MB (macOS only)")
	cmd.Flags().IntVar(&diskGB, "disk", 30, "appliance disk in GB (macOS only, first boot only)")
	return cmd
}

func newLocalDownCmd() *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop the local stack",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := loadLocalState()
			if err != nil {
				return err
			}
			switch runtime.GOOS {
			case "linux":
				if err := runLocalSetup(s, "stop"); err != nil {
					return err
				}
			case "darwin":
				if err := stopAppliance(s); err != nil {
					return err
				}
			default:
				return fmt.Errorf("fuse local supports linux and macOS, not %s", runtime.GOOS)
			}
			if purge {
				if err := os.RemoveAll(s.dir); err != nil {
					return err
				}
				successf("removed %s", s.dir)
			}
			successf("local stack stopped")
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete all local stack state (images, tokens, disks)")
	return cmd
}

func newLocalStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report the local stack's state",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := loadLocalState()
			if err != nil {
				return err
			}
			baseURL := ""
			switch runtime.GOOS {
			case "linux":
				_ = runLocalSetup(s, "status")
				baseURL = fmt.Sprintf("http://127.0.0.1:%d", localOrchPort)
			case "darwin":
				ip, err := applianceStatus(s)
				if err != nil || ip == "" {
					infof("appliance: not running")
					return nil
				}
				infof("appliance: running at %s", ip)
				baseURL = fmt.Sprintf("http://%s:%d", ip, localOrchPort)
			default:
				return fmt.Errorf("fuse local supports linux and macOS, not %s", runtime.GOOS)
			}
			if err := waitHTTPOK(cmd.Context(), baseURL+"/health", 2*time.Second); err != nil {
				infof("orchestrator: unreachable at %s", baseURL)
				return nil
			}
			infof("orchestrator: healthy at %s", baseURL)
			return nil
		},
	}
}

// localUpLinux brings the stack up directly on this machine: /dev/kvm,
// firecracker, and the agents all run here. install and start run under sudo
// because the agent needs tap devices and iptables.
func localUpLinux(ctx context.Context, s *localState) error {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("/dev/kvm not available: fuse local on linux needs kvm (%w)", err)
	}
	if err := injectDevOrchestrator(s.dir); err != nil {
		return err
	}
	infof("installing local stack under %s (downloads firecracker, a guest kernel/rootfs, and the fuse %s release on first run)", s.dir, localReleaseVersion())
	if err := runLocalSetup(s, "install"); err != nil {
		return err
	}
	if err := runLocalSetup(s, "start"); err != nil {
		return err
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", localOrchPort)
	agentURL := fmt.Sprintf("http://127.0.0.1:%d", localAgentPort)
	if err := waitHTTPOK(ctx, baseURL+"/health", 30*time.Second); err != nil {
		return fmt.Errorf("orchestrator did not become healthy: %w (see %s/orchestrator.log)", err, s.dir)
	}
	return connectLocal(ctx, s, baseURL, agentURL)
}

// runLocalSetup executes the embedded setup script under sudo with the stack
// env. stdin/stdout pass through so a sudo password prompt works.
func runLocalSetup(s *localState, subcommand string) error {
	cmd := exec.Command("sudo", "-E", "bash", s.setupScript, subcommand)
	cmd.Env = append(os.Environ(),
		"FUSE_LOCAL_DIR="+s.dir,
		"FC_AGENT_TOKEN="+s.fcAgentToken,
		"ORCH_AUTH_TOKEN="+s.orchAuthToken,
		"FUSE_VERSION="+localReleaseVersion(),
		fmt.Sprintf("ORCH_PORT=%d", localOrchPort),
		fmt.Sprintf("FC_AGENT_PORT=%d", localAgentPort),
		"PUBLIC_HOST=127.0.0.1",
	)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fuse-local-setup.sh %s: %w", subcommand, err)
	}
	return nil
}

// injectDevOrchestrator honors FUSE_LOCAL_ORCH_BIN: a path to a locally built
// linux orchestrator binary that should run instead of the release asset.
// install skips the download when bin/orchestrator already exists, so copying
// it into place first is the whole injection.
func injectDevOrchestrator(dir string) error {
	src := os.Getenv("FUSE_LOCAL_ORCH_BIN")
	if src == "" {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("FUSE_LOCAL_ORCH_BIN: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dir, "bin", "orchestrator")
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		return err
	}
	infof("using dev orchestrator binary from %s", src)
	return nil
}

// connectLocal registers this stack's host with the orchestrator (idempotent)
// and points the cli's "local" context at it.
func connectLocal(ctx context.Context, s *localState, baseURL, agentURL string) error {
	cl, err := fuse.New(baseURL, s.orchAuthToken, fuse.WithUserAgent("fuse-cli/"+version))
	if err != nil {
		return err
	}
	if _, err := cl.Hosts.Get(ctx, localHostID); err != nil {
		if _, err := cl.Hosts.Register(ctx, fuse.RegisterHostRequest{
			ID:      localHostID,
			URL:     agentURL,
			Token:   s.fcAgentToken,
			Backend: "firecracker",
			Capacity: fuse.HostCapacity{
				// cpus/ram/storage/arch are probed from the agent; vm_count
				// is policy and must be declared.
				VMCount: localMaxVMs,
			},
		}); err != nil {
			return fmt.Errorf("register local host: %w", err)
		}
		successf("registered host %q", localHostID)
	}

	app.cfg.Add(config.Context{
		Name:    localContextName,
		BaseURL: baseURL,
		Token:   s.orchAuthToken,
		Master:  true,
	})
	if err := app.cfg.Use(localContextName); err != nil {
		return err
	}
	if err := app.cfg.Save(); err != nil {
		return err
	}
	successf("connected: context %q -> %s", localContextName, baseURL)
	infof("try: fuse quickstart, or fuse up in a project with a Fusefile")
	return nil
}

// waitHTTPOK polls url until it answers 200 or the timeout lapses.
func waitHTTPOK(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("http %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("timed out after %s: %w", timeout, lastErr)
}
