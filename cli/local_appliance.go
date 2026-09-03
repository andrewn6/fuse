package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// the macOS side of fuse local: firecracker cannot run on darwin, so the
// stack runs inside a small linux appliance VM booted with vfkit under
// Apple's Virtualization.framework. nested virtualization (M3+, macOS 15+)
// gives the appliance /dev/kvm, so the microVMs inside it are real
// firecracker VMs — the same stack, agent, and wire behavior as a production
// host, with the appliance as an invisible boundary in between.
const (
	// applianceImageURL is a raw arm64 disk image (inside a tar.xz) with
	// cloud-init. debian publishes raw directly, which vfkit requires —
	// ubuntu cloud images are qcow2, which Virtualization.framework cannot
	// read.
	applianceImageURL = "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-arm64.tar.xz"

	// applianceMAC is fixed so the VM's DHCP lease (and thus its IP) can be
	// found in /var/db/dhcpd_leases. locally administered address.
	applianceMAC = "52:54:00:f5:5e:01"

	// applianceDir is where the stack lives inside the appliance.
	applianceDir = "/opt/fuse-local"

	applianceSSHUser = "fuse"
)

// localUpDarwin boots (or reuses) the appliance and brings the stack up
// inside it, then registers the host and connects the cli, exactly like the
// linux path does natively.
func localUpDarwin(ctx context.Context, s *localState, cpus, memMB, diskGB int) error {
	if runtime.GOARCH != "arm64" {
		return fmt.Errorf("fuse local on macOS needs Apple Silicon (M3 or later for nested virtualization); this machine is %s", runtime.GOARCH)
	}
	vfkit, err := exec.LookPath("vfkit")
	if err != nil {
		return fmt.Errorf("vfkit not found: install it with `brew install vfkit` (fuse local boots a linux appliance with it)")
	}
	if err := ensureApplianceSSHKey(s); err != nil {
		return err
	}
	if err := ensureApplianceDisk(s, diskGB); err != nil {
		return err
	}
	if err := writeCloudInit(s); err != nil {
		return err
	}
	if !vfkitRunning(s) {
		if err := startVfkit(s, vfkit, cpus, memMB); err != nil {
			return err
		}
	} else {
		infof("appliance already running")
	}

	infof("waiting for the appliance to boot...")
	ip, err := waitApplianceReady(ctx, s, 300*time.Second)
	if err != nil {
		return fmt.Errorf("appliance never came up: %w (console: %s)", err, s.applianceSerial)
	}
	infof("appliance is up at %s", ip)

	if err := injectDevOrchestratorAppliance(s, ip); err != nil {
		return err
	}

	infof("installing the stack inside the appliance (first run downloads firecracker, a guest kernel/rootfs, and the fuse %s release)", localReleaseVersion())
	if err := applianceSetup(s, ip, "install"); err != nil {
		return err
	}
	if err := applianceSetup(s, ip, "start"); err != nil {
		return err
	}

	baseURL := fmt.Sprintf("http://%s:%d", ip, localOrchPort)
	if err := waitHTTPOK(ctx, baseURL+"/health", 30*time.Second); err != nil {
		return fmt.Errorf("orchestrator did not become healthy: %w", err)
	}
	// the orchestrator reaches the agent inside the same VM, so its stored
	// url is loopback: it stays valid even if the appliance leases a new ip.
	agentURL := fmt.Sprintf("http://127.0.0.1:%d", localAgentPort)
	return connectLocal(ctx, s, baseURL, agentURL)
}

func ensureApplianceSSHKey(s *localState) error {
	if _, err := os.Stat(s.sshKey); err == nil {
		return nil
	}
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "fuse-local", "-f", s.sshKey)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("generate appliance ssh key: %w", err)
	}
	return nil
}

// ensureApplianceDisk downloads and unpacks the appliance image on first run,
// then grows the raw file to diskGB (sparse; cloud-init grows the partition
// inside on first boot).
func ensureApplianceDisk(s *localState, diskGB int) error {
	if _, err := os.Stat(s.applianceDisk); err == nil {
		return nil
	}
	infof("downloading appliance image (~210MB, first run only)")
	tarPath := filepath.Join(s.dir, "appliance.tar.xz")
	if err := runQuiet("curl", "-fSL", "--retry", "3", "-o", tarPath, applianceImageURL); err != nil {
		return fmt.Errorf("download appliance image: %w", err)
	}
	unpack := filepath.Join(s.dir, "appliance-unpack")
	if err := os.MkdirAll(unpack, 0o700); err != nil {
		return err
	}
	if err := runQuiet("tar", "-xf", tarPath, "-C", unpack); err != nil {
		return fmt.Errorf("unpack appliance image: %w", err)
	}
	raw, err := findRaw(unpack)
	if err != nil {
		return err
	}
	if err := os.Rename(raw, s.applianceDisk); err != nil {
		return err
	}
	_ = os.Remove(tarPath)
	_ = os.RemoveAll(unpack)
	if err := os.Truncate(s.applianceDisk, int64(diskGB)<<30); err != nil {
		return fmt.Errorf("grow appliance disk: %w", err)
	}
	return nil
}

func findRaw(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".raw") {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no .raw disk found in the appliance image archive")
}

// writeCloudInit renders user-data/meta-data. cloud-init only applies on the
// appliance's first boot; it creates the ssh user and writes the stack files
// (fc-agent.py, the setup script, the env file). install/start themselves run
// over ssh so first boot and every later boot share one code path.
func writeCloudInit(s *localState) error {
	pub, err := os.ReadFile(s.sshKey + ".pub")
	if err != nil {
		return err
	}
	fcAgent, err := os.ReadFile(s.fcAgentScript)
	if err != nil {
		return err
	}
	setup, err := os.ReadFile(s.setupScript)
	if err != nil {
		return err
	}
	env := fmt.Sprintf(
		"FUSE_LOCAL_DIR=%s\nFC_AGENT_TOKEN=%s\nORCH_AUTH_TOKEN=%s\nFUSE_VERSION=%s\nORCH_PORT=%d\nFC_AGENT_PORT=%d\nPUBLIC_HOST=auto\n",
		applianceDir, s.fcAgentToken, s.orchAuthToken, localReleaseVersion(), localOrchPort, localAgentPort)

	userData := fmt.Sprintf(`#cloud-config
hostname: fuse-local
users:
  - name: %s
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s
write_files:
  - path: %s/fc-agent.py
    encoding: b64
    content: %s
    permissions: "0644"
  - path: %s/fuse-local-setup.sh
    encoding: b64
    content: %s
    permissions: "0755"
  - path: %s/env
    encoding: b64
    content: %s
    permissions: "0600"
`,
		applianceSSHUser,
		strings.TrimSpace(string(pub)),
		applianceDir, base64.StdEncoding.EncodeToString(fcAgent),
		applianceDir, base64.StdEncoding.EncodeToString(setup),
		applianceDir, base64.StdEncoding.EncodeToString([]byte(env)),
	)
	if err := os.WriteFile(filepath.Join(s.dir, "user-data"), []byte(userData), 0o600); err != nil {
		return err
	}
	metaData := "instance-id: fuse-local\nlocal-hostname: fuse-local\n"
	return os.WriteFile(filepath.Join(s.dir, "meta-data"), []byte(metaData), 0o600)
}

func vfkitRunning(s *localState) bool {
	pid := readPid(s.vfkitPid)
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func readPid(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

func startVfkit(s *localState, vfkit string, cpus, memMB int) error {
	args := []string{
		"--cpus", strconv.Itoa(cpus),
		"--memory", strconv.Itoa(memMB),
		"--bootloader", "efi,variable-store=" + filepath.Join(s.dir, "efistore.nvram") + ",create",
		"--device", "virtio-blk,path=" + s.applianceDisk,
		"--cloud-init", filepath.Join(s.dir, "user-data") + "," + filepath.Join(s.dir, "meta-data"),
		"--device", "virtio-net,nat,mac=" + applianceMAC,
		"--device", "virtio-serial,logFilePath=" + s.applianceSerial,
		"--device", "virtio-rng",
		"--nested",
	}
	cmd := exec.Command(vfkit, args...)
	logf, err := os.OpenFile(filepath.Join(s.dir, "vfkit.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = logf.Close() }()
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start vfkit: %w", err)
	}
	if err := os.WriteFile(s.vfkitPid, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		return err
	}
	if err := cmd.Process.Release(); err != nil {
		return err
	}
	// a wrong flag (e.g. a vfkit too old for --nested) exits immediately;
	// catch that here instead of timing out on DHCP later.
	time.Sleep(1500 * time.Millisecond)
	if !vfkitRunning(s) {
		out, _ := os.ReadFile(filepath.Join(s.dir, "vfkit.log"))
		tail := string(out)
		if len(tail) > 800 {
			tail = tail[len(tail)-800:]
		}
		return fmt.Errorf("vfkit exited immediately (nested virtualization needs an M3 or later on macOS 15+, and a recent vfkit):\n%s", tail)
	}
	infof("appliance booting (vfkit pid %d)", readPid(s.vfkitPid))
	return nil
}

func stopAppliance(s *localState) error {
	pid := readPid(s.vfkitPid)
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		infof("appliance not running")
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	for i := 0; i < 20; i++ {
		if syscall.Kill(pid, 0) != nil {
			_ = os.Remove(s.vfkitPid)
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	_ = os.Remove(s.vfkitPid)
	return nil
}

// applianceStatus reports the appliance ip if the vm is running, "" otherwise.
func applianceStatus(s *localState) (string, error) {
	if !vfkitRunning(s) {
		return "", nil
	}
	ips, err := candidateLeaseIPs(applianceMAC)
	if err != nil || len(ips) == 0 {
		return "", err
	}
	return ips[0], nil
}

// waitApplianceReady returns the appliance's ip once ssh actually answers on
// it.
//
// Resolving the lease and waiting for ssh used to be two steps, and that is
// what made a rebuilt appliance hang: the lease table still holds the previous
// generation's "fuse-local" entry, the new one has not been issued yet, so the
// first step returned a dead ip immediately and the second spent its whole
// timeout on it. There is no ordering rule that fixes this, because at the
// moment of the lookup the right answer is not in the file at all.
//
// So nothing is committed to until it answers. Every tick re-reads the lease
// table, which is cheap, and probes the candidates best-first. A stale entry
// costs one failed dial per second instead of the entire budget, and the live
// lease is picked up as soon as macOS writes it.
func waitApplianceReady(ctx context.Context, s *localState, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if !vfkitRunning(s) {
			out, _ := os.ReadFile(filepath.Join(s.dir, "vfkit.log"))
			tail := string(out)
			if len(tail) > 800 {
				tail = tail[len(tail)-800:]
			}
			return "", fmt.Errorf("vfkit exited:\n%s", tail)
		}
		ips, err := candidateLeaseIPs(applianceMAC)
		if err != nil {
			lastErr = err
		}
		for _, ip := range ips {
			args := append(applianceSSHArgs(s, ip), "true")
			if err := exec.CommandContext(ctx, "ssh", args...).Run(); err == nil {
				return ip, nil
			} else {
				lastErr = err
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no lease for %s appeared", applianceMAC)
	}
	return "", fmt.Errorf("timed out after %s: %w", timeout, lastErr)
}

// findLeaseIP scans /var/db/dhcpd_leases for the appliance's entry. two
// match paths, because what lands in hw_address depends on the guest's dhcp
// client: a plain MAC ("1,52:54:0:f5:5e:1", octets stored without leading
// zeros), or an opaque DUID client-identifier ("ff,f1:f5:..."), which debian
// sends and which cannot be predicted. the DUID case is matched by name
// instead — the lease name is the hostname we set via cloud-init meta-data.
//
// a rebuilt appliance boots from a fresh disk, so it sends a fresh DUID and
// macOS hands it a NEW ip while leaving the previous "fuse-local" lease in the
// file. that means matches are not unique and the file order cannot be trusted
// to put the live one first: it does not exist yet at the moment we look.
// collect every candidate and let pickLease decide.
type dhcpLease struct {
	ip string
	// mac reports whether this entry matched the pinned appliance MAC rather
	// than the hostname. a MAC match identifies the appliance exactly; a name
	// match can be any generation of it.
	mac bool
	// expiry is the lease= field, an absolute unix time in hex. zero when the
	// entry carried no parseable lease.
	expiry int64
}

// findLeaseIPIn returns the single best candidate. Callers that are about to
// connect should prefer candidateLeaseIPs and probe, since the best candidate
// is only a guess until something answers on it.
func findLeaseIPIn(path, mac, name string) (string, error) {
	ips, err := candidateLeaseIPsIn(path, mac, name)
	if err != nil || len(ips) == 0 {
		return "", err
	}
	return ips[0], nil
}

func scanLeases(path, mac, name string) ([]dhcpLease, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	want := normalizeMAC(mac)
	var found []dhcpLease
	var cur dhcpLease
	var named bool
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "{":
			cur, named = dhcpLease{}, false
		case strings.HasPrefix(line, "name="):
			named = strings.TrimPrefix(line, "name=") == name
		case strings.HasPrefix(line, "ip_address="):
			cur.ip = strings.TrimPrefix(line, "ip_address=")
		case strings.HasPrefix(line, "lease="):
			v := strings.TrimPrefix(strings.TrimPrefix(line, "lease="), "0x")
			if n, err := strconv.ParseInt(v, 16, 64); err == nil {
				cur.expiry = n
			}
		case strings.HasPrefix(line, "hw_address="):
			// "1,52:54:0:f5:5e:1" — the leading 1 is the hardware type.
			if _, addr, ok := strings.Cut(strings.TrimPrefix(line, "hw_address="), ","); ok && normalizeMAC(addr) == want {
				cur.mac = true
			}
		case line == "}":
			if cur.ip != "" && (cur.mac || named) {
				found = append(found, cur)
			}
		}
	}
	return found, nil
}

// orderLeases sorts candidates into the order worth probing.
//
// Two tiers. A MAC match is exact identity and outranks a hostname match,
// which can name any generation of the appliance. Within a tier the newest
// lease= wins, because that is the only field saying which entry macOS issued
// most recently. Ties keep file order, which is what makes entries carrying no
// lease= at all behave the way they always have.
//
// Expired leases stay in the list. The clock belongs to the host rather than
// the guest, and since every candidate is probed before use, a wrong guess is
// now one failed dial instead of a failed boot.
func orderLeases(found []dhcpLease) []dhcpLease {
	out := append([]dhcpLease(nil), found...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].mac != out[j].mac {
			return out[i].mac
		}
		return out[i].expiry > out[j].expiry
	})
	return out
}

// candidateLeaseIPs returns every ip that could be the appliance, best first.
func candidateLeaseIPs(mac string) ([]string, error) {
	return candidateLeaseIPsIn("/var/db/dhcpd_leases", mac, "fuse-local")
}

func candidateLeaseIPsIn(path, mac, name string) ([]string, error) {
	found, err := scanLeases(path, mac, name)
	if err != nil {
		return nil, err
	}
	var ips []string
	seen := map[string]bool{}
	for _, l := range orderLeases(found) {
		if !seen[l.ip] {
			seen[l.ip] = true
			ips = append(ips, l.ip)
		}
	}
	return ips, nil
}

func normalizeMAC(mac string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(mac)), ":")
	for i, p := range parts {
		parts[i] = strings.TrimLeft(p, "0")
		if parts[i] == "" {
			parts[i] = "0"
		}
	}
	return strings.Join(parts, ":")
}

func applianceSSHArgs(s *localState, ip string) []string {
	return []string{
		"-i", s.sshKey,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		applianceSSHUser + "@" + ip,
	}
}

// applianceSetup runs the embedded setup script inside the appliance with the
// env file cloud-init wrote, streaming output through so downloads are
// visible.
func applianceSetup(s *localState, ip, subcommand string) error {
	remote := fmt.Sprintf(
		"sudo bash -c 'set -a; . %s/env; set +a; bash %s/fuse-local-setup.sh %s'",
		applianceDir, applianceDir, subcommand)
	args := append(applianceSSHArgs(s, ip), remote)
	cmd := exec.Command("ssh", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("appliance setup %s: %w", subcommand, err)
	}
	return nil
}

// injectDevOrchestratorAppliance copies a locally built linux/arm64
// orchestrator (FUSE_LOCAL_ORCH_BIN) into the appliance so install skips the
// release download. the dev loop for working on the orchestrator itself.
func injectDevOrchestratorAppliance(s *localState, ip string) error {
	src := os.Getenv("FUSE_LOCAL_ORCH_BIN")
	if src == "" {
		return nil
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("FUSE_LOCAL_ORCH_BIN: %w", err)
	}
	mkdir := append(applianceSSHArgs(s, ip), "sudo mkdir -p "+applianceDir+"/bin")
	if err := exec.Command("ssh", mkdir...).Run(); err != nil {
		return fmt.Errorf("prepare appliance bin dir: %w", err)
	}
	push := append(applianceSSHArgs(s, ip),
		"sudo bash -c 'cat > "+applianceDir+"/bin/orchestrator && chmod 0755 "+applianceDir+"/bin/orchestrator'")
	cmd := exec.Command("ssh", push...)
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	cmd.Stdin = f
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("push dev orchestrator: %w: %s", err, out)
	}
	// the version marker install reads to decide whether to re-download. "dev"
	// is the one value it will not overwrite, so a dev binary survives every
	// later `fuse local up` instead of being replaced by the release build.
	mark := append(applianceSSHArgs(s, ip),
		"sudo bash -c 'echo dev > "+applianceDir+"/bin/.orchestrator-version'")
	if out, err := exec.Command("ssh", mark...).CombinedOutput(); err != nil {
		return fmt.Errorf("mark dev orchestrator: %w: %s", err, out)
	}
	infof("using dev orchestrator binary from %s", src)
	return nil
}

func runQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
