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
	// entries are newest-first, so the stale fuse-local entry loses.
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
