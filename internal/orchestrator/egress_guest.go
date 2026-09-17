package orchestrator

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/folsomintel/fuse/internal/egress"
)

// GuestEgressDir is where the guest learns its egress policy as data: a
// readable directory outside ReservedGuestDir, because /fuse is mode 0700
// and holds credentials, while the proxy endpoint is not a secret and a
// non-root browser harness has to be able to read it. the api refuses a
// caller file under it for the same reason it refuses one under /fuse.
const GuestEgressDir = "/etc/fuse"

// guest paths for the egress policy. all three are written on every boot,
// direct included, so a harness can tell "not proxied" from "predates this
// file" and so every consumer below can source or reference them
// unconditionally.
const (
	// GuestEgressJSONPath is the policy as data, for browser automation and
	// anything else that wants the endpoint rather than an inherited
	// variable. fused reads it for /v1/info.
	GuestEgressJSONPath = GuestEgressDir + "/egress.json"
	// GuestEgressEnvPath holds the proxy variables as KEY=value lines: the
	// single source both the shell hook and compose read. empty for direct,
	// because an empty HTTP_PROXY is not the same as an unset one to every
	// client that reads it.
	GuestEgressEnvPath = GuestEgressDir + "/egress.env"
	// GuestEgressProfilePath exports the env file into login shells, which
	// covers the startup script (sh -lc), `fuse exec --shell` and a bare
	// attach. the host agents source it explicitly for non-login exec too.
	GuestEgressProfilePath = "/etc/profile.d/fuse-egress.sh"
)

// GuestEgress is the shape of GuestEgressJSONPath. the json tags are the
// guest wire, shared with fused.
type GuestEgress struct {
	Mode     string `json:"mode"`
	Provider string `json:"provider,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	// NoProxy is the bypass set the variables carry, as bare ips: the vm's
	// loopback (compose publishes every service port there), the guest's own
	// tap address and the host side of the tap. many clients silently
	// ignore cidr in NO_PROXY, so a /16 can look like coverage and provide
	// none.
	NoProxy []string `json:"no_proxy,omitempty"`
}

// egressGuestFiles renders the three guest files for a resolved policy.
// listenIP and guestIP are the two ends of the tap, empty when the
// environment could not report them.
func egressGuestFiles(status egress.Status, listenIP, guestIP string) map[string][]byte {
	mode := status.Mode
	if mode == "" {
		mode = egress.ModeDirect
	}
	doc := GuestEgress{Mode: string(mode)}
	var env strings.Builder
	if mode == egress.ModeProxy {
		doc.Provider = status.Provider
		doc.Protocol = string(status.Protocol)
		doc.Endpoint = status.Endpoint
		doc.NoProxy = egressNoProxy(listenIP, guestIP)
		// both cases, because the ecosystem is split: curl reads lowercase,
		// go reads either, some tools read only uppercase. one case
		// reliably misses something.
		vars := [][2]string{
			{"HTTP_PROXY", status.Endpoint},
			{"HTTPS_PROXY", status.Endpoint},
			{"ALL_PROXY", status.Endpoint},
			{"NO_PROXY", strings.Join(doc.NoProxy, ",")},
		}
		for _, kv := range vars {
			fmt.Fprintf(&env, "%s=%s\n", kv[0], kv[1])
			fmt.Fprintf(&env, "%s=%s\n", strings.ToLower(kv[0]), kv[1])
		}
	}
	docJSON, _ := json.Marshal(doc)
	profile := "# written by the fuse orchestrator; exports the environment's egress policy.\n" +
		"[ -r " + GuestEgressEnvPath + " ] || return 0 2>/dev/null || exit 0\n" +
		"set -a\n. " + GuestEgressEnvPath + "\nset +a\n"
	return map[string][]byte{
		GuestEgressJSONPath:    docJSON,
		GuestEgressEnvPath:     []byte(env.String()),
		GuestEgressProfilePath: []byte(profile),
	}
}

// egressNoProxy is the bypass set: loopback in every spelling, then the
// tap's two ends when known, deduplicated and sorted so the file is stable.
func egressNoProxy(listenIP, guestIP string) []string {
	set := map[string]struct{}{"localhost": {}, "127.0.0.1": {}, "::1": {}}
	for _, ip := range []string{listenIP, guestIP} {
		if ip != "" {
			set[ip] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for ip := range set {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

// withEgressFiles returns a copy of files with the egress guest files added.
// a copy, because AgentSpec.Files is shared by the restore and fresh paths
// and the endpoint differs per boot.
func withEgressFiles(files map[string][]byte, status egress.Status, env Environment) map[string][]byte {
	out := make(map[string][]byte, len(files)+3)
	for path, data := range files {
		out[path] = data
	}
	listenIP, guestIP := egressAddresses(env)
	for path, data := range egressGuestFiles(status, listenIP, guestIP) {
		out[path] = data
	}
	return out
}
