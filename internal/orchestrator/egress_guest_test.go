package orchestrator

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/egress"
)

func TestEgressGuestFilesDirect(t *testing.T) {
	files := egressGuestFiles(egress.Status{Mode: egress.ModeDirect}, "10.200.3.1", "10.200.3.2")
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3", len(files))
	}
	// a direct environment gets a valid document saying so, so a harness
	// can tell "not proxied" from "predates this file".
	var doc GuestEgress
	if err := json.Unmarshal(files[GuestEgressJSONPath], &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Mode != "direct" || doc.Endpoint != "" || len(doc.NoProxy) != 0 {
		t.Fatalf("direct doc = %+v", doc)
	}
	// and none of the variables set: an empty HTTP_PROXY is not the same
	// as an unset one to every client that reads it.
	if got := string(files[GuestEgressEnvPath]); got != "" {
		t.Fatalf("direct env file = %q, want empty", got)
	}
	if !strings.Contains(string(files[GuestEgressProfilePath]), ". "+GuestEgressEnvPath) {
		t.Fatalf("profile hook does not source the env file: %q", files[GuestEgressProfilePath])
	}
}

func TestEgressGuestFilesProxy(t *testing.T) {
	status := egress.Status{Mode: egress.ModeProxy, Provider: "mock", Protocol: egress.ProtocolSOCKS5, Endpoint: "socks5h://10.200.3.1:1080"}
	files := egressGuestFiles(status, "10.200.3.1", "10.200.3.2")

	var doc GuestEgress
	if err := json.Unmarshal(files[GuestEgressJSONPath], &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Mode != "proxy" || doc.Provider != "mock" || doc.Protocol != "socks5" || doc.Endpoint != status.Endpoint {
		t.Fatalf("proxy doc = %+v", doc)
	}
	wantNoProxy := []string{"10.200.3.1", "10.200.3.2", "127.0.0.1", "::1", "localhost"}
	if strings.Join(doc.NoProxy, ",") != strings.Join(wantNoProxy, ",") {
		t.Fatalf("no_proxy = %v, want %v", doc.NoProxy, wantNoProxy)
	}

	env := string(files[GuestEgressEnvPath])
	// both cases of every variable: the ecosystem is split on which one
	// it reads.
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if !strings.Contains(env, name+"="+status.Endpoint+"\n") {
			t.Errorf("env file lacks %s=%s:\n%s", name, status.Endpoint, env)
		}
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		if !strings.Contains(env, name+"="+strings.Join(wantNoProxy, ",")+"\n") {
			t.Errorf("env file lacks %s bypass set:\n%s", name, env)
		}
	}
	// bare ips, never cidr: many clients silently ignore cidr in NO_PROXY.
	if strings.Contains(env, "/") && !strings.Contains(env, "://") {
		t.Errorf("env file carries a cidr:\n%s", env)
	}
}

func TestEgressGuestFilesWithoutAddresses(t *testing.T) {
	status := egress.Status{Mode: egress.ModeProxy, Provider: "mock", Endpoint: "socks5h://10.200.3.1:1080"}
	files := egressGuestFiles(status, "", "")
	var doc GuestEgress
	if err := json.Unmarshal(files[GuestEgressJSONPath], &doc); err != nil {
		t.Fatal(err)
	}
	// an environment that could not report its tap still bypasses loopback.
	if strings.Join(doc.NoProxy, ",") != "127.0.0.1,::1,localhost" {
		t.Fatalf("no_proxy = %v", doc.NoProxy)
	}
}

func TestWithEgressFilesDoesNotAliasTheProfile(t *testing.T) {
	base := map[string][]byte{fuseManifestPath: []byte("{}")}
	out := withEgressFiles(base, egress.Status{Mode: egress.ModeDirect}, &mockEnv{})
	if len(base) != 1 {
		t.Fatalf("withEgressFiles mutated the shared profile map: %d entries", len(base))
	}
	if len(out) != 4 || out[GuestEgressJSONPath] == nil {
		t.Fatalf("merged files = %d entries", len(out))
	}
}

func TestComposeServicesReferenceTheEgressEnvFile(t *testing.T) {
	manifest := []byte(`{"version":"1","services":{"web":{"image":"nginx","ports":[8080]}}}`)
	compose, ok := composeFromManifest(manifest, nil)
	if !ok {
		t.Fatal("compose not rendered")
	}
	if !strings.Contains(string(compose), "env_file:\n") || !strings.Contains(string(compose), "- "+GuestEgressEnvPath) {
		t.Fatalf("compose lacks the egress env_file:\n%s", compose)
	}
}
