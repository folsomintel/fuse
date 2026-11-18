package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"strings"
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

// every up pushes the stack files into the appliance as one tar; it must carry
// what fc-agent.py and its image convert need, plus the env file, with the
// modes install relies on.
func TestApplianceTarCarriesTheStackFiles(t *testing.T) {
	s := &localState{fcAgentToken: "agent-tok", orchAuthToken: "orch-tok"}
	data, err := applianceTar(s)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*tar.Header{}
	body := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		got[hdr.Name], body[hdr.Name] = hdr, b
	}
	want := map[string]int64{
		"fuse-local-setup.sh":      0o755,
		"fc-agent.py":              0o644,
		"agentlib.py":              0o644,
		"fc-bake-distro-rootfs.sh": 0o755,
		"fc-bake-lib.sh":           0o644,
		"fused.service":            0o644,
		"env":                      0o600,
	}
	for name, mode := range want {
		hdr, ok := got[name]
		if !ok {
			t.Errorf("tar is missing %s", name)
			continue
		}
		if hdr.Mode != mode {
			t.Errorf("%s: mode %o, want %o", name, hdr.Mode, mode)
		}
		if len(body[name]) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("tar has %d files, want %d", len(got), len(want))
	}
	env := string(body["env"])
	for _, line := range []string{"FC_AGENT_TOKEN=agent-tok\n", "ORCH_AUTH_TOKEN=orch-tok\n", "FUSE_VERSION=" + localReleaseVersion() + "\n"} {
		if !strings.Contains(env, line) {
			t.Errorf("env is missing %q:\n%s", line, env)
		}
	}
}

// the assets are embedded byte-exact from the files a fleet host runs, so the
// local stack bakes images exactly the way a fleet host does.
func TestLocalAssetsMatchTheRepoFiles(t *testing.T) {
	src := map[string]string{
		"fc-agent.py":              "../host-agent/firecracker/fc-agent.py",
		"agentlib.py":              "../host-agent/shared/agentlib.py",
		"fuse-local-setup.sh":      "../host-agent/local/fuse-local-setup.sh",
		"fc-bake-distro-rootfs.sh": "../host-agent/firecracker/scripts/fc-bake-distro-rootfs.sh",
		"fc-bake-lib.sh":           "../host-agent/firecracker/scripts/fc-bake-lib.sh",
		"fused.service":            "../host-agent/firecracker/fused.service",
	}
	for _, a := range localAssets() {
		path, ok := src[a.name]
		if !ok {
			t.Errorf("unexpected asset %s", a.name)
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a.data, b) {
			t.Errorf("%s differs from %s", a.name, path)
		}
	}
}

// cloud-init only runs on first boot, so it must not be where the stack files
// come from: a file written there is never refreshed by a cli upgrade.
func TestCloudInitOnlyCreatesTheUser(t *testing.T) {
	dir := t.TempDir()
	s := &localState{dir: dir, sshKey: dir + "/key"}
	if err := os.WriteFile(s.sshKey+".pub", []byte("ssh-ed25519 AAAA test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeCloudInit(s); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dir + "/user-data")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "write_files") {
		t.Errorf("user-data still writes files:\n%s", b)
	}
	if !strings.Contains(string(b), "ssh-ed25519 AAAA test") {
		t.Errorf("user-data lacks the ssh key:\n%s", b)
	}
}
