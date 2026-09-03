package main

import (
	"os"
	"testing"
)

func TestNormalizeMAC(t *testing.T) {
	// /var/db/dhcpd_leases strips leading zeros per octet; both spellings of
	// the same address must normalize identically.
	if normalizeMAC("52:54:00:f5:5e:01") != normalizeMAC("52:54:0:f5:5e:1") {
		t.Error("zero-stripped and padded MACs should match")
	}
	if normalizeMAC("52:54:00:f5:5e:01") == normalizeMAC("52:54:00:f5:5e:02") {
		t.Error("different MACs should not match")
	}
}

func TestLocalReleaseVersion(t *testing.T) {
	old := version
	defer func() { version = old }()

	version = "dev"
	if got := localReleaseVersion(); got != "latest" {
		t.Errorf("dev build: got %q, want latest", got)
	}
	version = "0.14.0"
	if got := localReleaseVersion(); got != "v0.14.0" {
		t.Errorf("got %q, want v0.14.0", got)
	}
	version = "v0.14.0"
	if got := localReleaseVersion(); got != "v0.14.0" {
		t.Errorf("already-prefixed: got %q, want v0.14.0", got)
	}
}

func TestFindLeaseIP(t *testing.T) {
	dir := t.TempDir()
	leases := dir + "/dhcpd_leases"

	// debian's dhcp client sends a DUID client-identifier, so hw_address is
	// not the mac; the entry is found by its cloud-init hostname instead.
	// neither entry carries a lease=, so they tie and file order decides.
	duid := `{
	name=fuse-local
	ip_address=192.168.64.3
	hw_address=ff,f1:f5:dd:7f:0:2:0:0:ab:11:a5:e3:4:c1:ea:a3:ed:25
}
{
	name=other-vm
	ip_address=192.168.64.2
	hw_address=1,aa:bb:cc:0:11:22
}
{
	name=fuse-local
	ip_address=192.168.64.9
	hw_address=ff,de:ad:be:ef:0:1
}
`
	if err := os.WriteFile(leases, []byte(duid), 0o600); err != nil {
		t.Fatal(err)
	}
	ip, err := findLeaseIPIn(leases, "52:54:00:f5:5e:01", "fuse-local")
	if err != nil || ip != "192.168.64.3" {
		t.Errorf("duid lease: ip=%q err=%v, want 192.168.64.3", ip, err)
	}

	// a plain-mac client matches by normalized hw_address even when the file
	// strips leading zeros.
	mac := `{
	name=whatever
	ip_address=192.168.64.7
	hw_address=1,52:54:0:f5:5e:1
}
`
	if err := os.WriteFile(leases, []byte(mac), 0o600); err != nil {
		t.Fatal(err)
	}
	ip, err = findLeaseIPIn(leases, "52:54:00:f5:5e:01", "fuse-local")
	if err != nil || ip != "192.168.64.7" {
		t.Errorf("mac lease: ip=%q err=%v, want 192.168.64.7", ip, err)
	}
}

// Verbatim shape of /var/db/dhcpd_leases after rebuilding the appliance: the
// fresh disk sends a new DUID, so macOS leases it a second address and keeps
// the old "fuse-local" entry. Picking the stale one sends `fuse local up` to
// an ip that does not answer and burns the full ssh timeout before failing.
func TestFindLeaseIPPrefersLiveAppliance(t *testing.T) {
	dir := t.TempDir()
	leases := dir + "/dhcpd_leases"

	const (
		newer = "0x6a97b81e" // the rebuilt appliance, actually reachable
		older = "0x6a97a90b" // the previous generation, gone
	)

	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(leases, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	live := `{
	name=fuse-local
	ip_address=192.168.64.4
	hw_address=ff,f1:f5:dd:7f:0:2:0:0:ab:11:42:d9:69:3a:a8:de:8:a8
	lease=` + newer + `
}
`
	stale := `{
	name=fuse-local
	ip_address=192.168.64.3
	hw_address=ff,f1:f5:dd:7f:0:2:0:0:ab:11:a5:e3:4:c1:ea:a3:ed:25
	lease=` + older + `
}
`
	unrelated := `{
	name=alarm
	ip_address=192.168.64.2
	hw_address=ff,f1:f5:dd:7f:0:2:0:0:ab:11:4d:61:be:2d:76:c:84:66
	lease=` + older + `
}
`

	// Both orderings, because file order is exactly what cannot be trusted:
	// at the moment of the lookup the live lease may not be written yet.
	for _, tc := range []struct{ name, body string }{
		{"live entry first", live + stale + unrelated},
		{"stale entry first", stale + live + unrelated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(tc.body)
			ip, err := findLeaseIPIn(leases, "52:54:00:f5:5e:01", "fuse-local")
			if err != nil || ip != "192.168.64.4" {
				t.Errorf("ip=%q err=%v, want 192.168.64.4", ip, err)
			}
		})
	}

	// A pinned-MAC entry identifies the appliance exactly, so it should win
	// over a hostname match even when the hostname match holds a newer lease.
	t.Run("mac match beats a newer name match", func(t *testing.T) {
		write(`{
	name=fuse-local
	ip_address=192.168.64.8
	hw_address=ff,de:ad:be:ef:0:1
	lease=` + newer + `
}
{
	name=someone-else
	ip_address=192.168.64.5
	hw_address=1,52:54:0:f5:5e:1
	lease=` + older + `
}
`)
		ip, err := findLeaseIPIn(leases, "52:54:00:f5:5e:01", "fuse-local")
		if err != nil || ip != "192.168.64.5" {
			t.Errorf("ip=%q err=%v, want 192.168.64.5", ip, err)
		}
	})

	t.Run("no match is not an error", func(t *testing.T) {
		write(unrelated)
		ip, err := findLeaseIPIn(leases, "52:54:00:f5:5e:01", "fuse-local")
		if err != nil || ip != "" {
			t.Errorf("ip=%q err=%v, want empty", ip, err)
		}
	})

	// The failure that made `fuse local up` hang: at lookup time the rebuilt
	// appliance has not been issued a lease yet, so the only candidate on offer
	// is the dead previous generation. No ranking can fix that, which is why
	// the caller probes every candidate instead of trusting the top one. Both
	// must therefore be offered, stale one included.
	t.Run("every candidate is offered, not just the best", func(t *testing.T) {
		write(stale + live)
		ips, err := candidateLeaseIPsIn(leases, "52:54:00:f5:5e:01", "fuse-local")
		if err != nil {
			t.Fatal(err)
		}
		if len(ips) != 2 || ips[0] != "192.168.64.4" || ips[1] != "192.168.64.3" {
			t.Errorf("ips=%v, want [192.168.64.4 192.168.64.3]", ips)
		}
	})

	// Two entries can share an ip across generations; probing the same address
	// twice per tick is wasted time on the one path that is already slow.
	t.Run("duplicate ips collapse", func(t *testing.T) {
		write(live + live)
		ips, err := candidateLeaseIPsIn(leases, "52:54:00:f5:5e:01", "fuse-local")
		if err != nil {
			t.Fatal(err)
		}
		if len(ips) != 1 || ips[0] != "192.168.64.4" {
			t.Errorf("ips=%v, want [192.168.64.4]", ips)
		}
	})
}
