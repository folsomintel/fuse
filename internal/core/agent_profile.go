package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/surf-dev/surf/apps/orchestrator/secrets"
)

// This file is the SINGLE home for all surfd-specific knowledge. The core
// Boot/drain/provider code is generic: it uploads AgentSpec.Files, launches
// AgentSpec.Command (or honors structured fields a provider chooses to read),
// and runs AgentSpec.DrainCommand for graceful shutdown. The surfd profile
// below reproduces the orchestrator's original behavior — the /surf/* guest
// paths and the surfd launch command line live here and nowhere else.

// surfd guest paths. These follow the surfd `/surf/` convention (see PRD-08):
// the guest fc-agent mounts /surf on tmpfs so secrets never reach persistent
// storage. They are profile details, not core defaults.
const (
	surfManifestPath  = "/surf/manifest.json"
	surfSecretsPath   = "/surf/secrets.json"
	surfTLSCertPath   = "/surf/tls/cert.pem"
	surfTLSKeyPath    = "/surf/tls/key.pem"
	surfAuthTokenPath = "/surf/auth-token"
)

// SurfSecretsPath is exported so fleet.reuploadSecrets can re-upload to the
// profile-declared path without hardcoding the literal.
const SurfSecretsPath = surfSecretsPath

// surfAgentBinaryPath is where the surfd profile expects the daemon binary to
// live inside the guest. It must match the binary path referenced by the
// command line SurfdAgentSpec assembles (and the destination a download-based
// provider writes to).
const surfAgentBinaryPath = "/usr/local/bin/surfd"

// DefaultSurfdDrainCommand is the surfd profile's graceful-stop invocation, run
// inside the guest via Environment.ExecStream on Drain.
//
// surfd traps SIGTERM and tears down its service DAG before exiting — the exact
// teardown the original gRPC Down RPC drove (see surfd main.go signal handling →
// Daemon.Run → env.Down). So stopping surfd with a signal IS a full graceful
// drain. On the firecracker profile surfd is a systemd unit, so `systemctl stop`
// delivers SIGTERM, waits for the clean exit, and (because the stop is clean)
// the unit's Restart=on-failure does NOT bring it back. The `pkill -TERM`
// fallback covers non-systemd guests, which also quiesce on SIGTERM.
//
// No trailing `|| true`: a genuine failure (neither a unit nor a process was
// stopped) propagates a non-zero exit so Drain records the error and keeps the
// VM Draining, matching the original Down-RPC error semantics. This replaced
// `surfctl down || true`, which silently no-op'd because surfctl is not present
// in the guest image and `|| true` swallowed the failure.
const DefaultSurfdDrainCommand = "systemctl stop surfd 2>/dev/null || pkill -TERM surfd"

// DefaultSurfdManifest is the surfd profile's default compiled manifest, used
// by the API when a caller omits an inline manifest. It encodes the surfd
// manifest schema and is profile data, not a generic core default.
var DefaultSurfdManifest = []byte(`{"version":"1","machine":{"workspace":"/workspace"},"services":{}}`)

// surfdCredentialFiles returns the surfd profile's per-VM credential files
// (TLS cert, TLS key, auth token) keyed by guest path, or nil when creds is
// nil. Reused by SurfdAgentSpec (fresh boot) and token rotation (re-upload).
func surfdCredentialFiles(creds *secrets.VMCredentials) map[string][]byte {
	if creds == nil {
		return nil
	}
	return map[string][]byte{
		surfTLSCertPath:   creds.CertPEM,
		surfTLSKeyPath:    creds.KeyPEM,
		surfAuthTokenPath: []byte(creds.AuthToken),
	}
}

// SurfdAgentSpec builds the surfd profile: the full AgentSpec that reproduces
// the orchestrator's original boot behavior. Files carries the manifest, the
// secrets JSON (defaulting to "{}" when nil/empty), and — when creds is set —
// the TLS/auth credential files. Command is the surfd launch line.
//
// NOTE on Command: the Command field is the generic launch line for providers
// that run a free-form shell command. The firecracker host agent IGNORES
// Command and instead reads structured fields off the frozen /start-surfd wire
// (manifest/secrets/TLS paths), sourcing them from its own path constants that
// mirror the consts above.
func SurfdAgentSpec(manifest []byte, secretMap map[string]string, creds *secrets.VMCredentials, opts BootOptions) AgentSpec {
	files := map[string][]byte{
		surfManifestPath: manifest,
	}

	// Default to an empty secret map so the guest always finds a valid
	// /surf/secrets.json (this is the empty-map default logic, centralized
	// here in the profile).
	secretsMap := secretMap
	if secretsMap == nil {
		secretsMap = map[string]string{}
	}
	secretsJSON, _ := json.Marshal(secretsMap)
	files[surfSecretsPath] = secretsJSON

	for path, data := range surfdCredentialFiles(creds) {
		files[path] = data
	}

	spec := AgentSpec{
		Files:        files,
		Command:      buildSurfdCommand(creds, opts),
		Gateway:      opts.GatewayURL,
		GatewayToken: opts.GatewayToken,
		DrainCommand: DefaultSurfdDrainCommand,
	}
	if creds != nil {
		spec.AuthToken = creds.AuthToken
	}
	return spec
}

// buildSurfdCommand reconstructs the surfd launch command line that the
// orchestrator originally produced. SURF_CONTAINER_BIN=/bin/true is set
// because some sandbox images have no container runtime. When credentials are
// present the daemon runs with TLS + a file-backed auth token; otherwise it
// runs --insecure. Gateway values are shell-escaped to match prior behavior.
func buildSurfdCommand(creds *secrets.VMCredentials, opts BootOptions) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "SURF_CONTAINER_BIN=/bin/true %s", surfAgentBinaryPath)
	b.WriteString(" --listen 0.0.0.0:3000")
	fmt.Fprintf(&b, " --manifest %s", surfManifestPath)
	fmt.Fprintf(&b, " --secrets %s", surfSecretsPath)
	if creds != nil {
		fmt.Fprintf(&b, " --auth-token-file %s", surfAuthTokenPath)
		fmt.Fprintf(&b, " --tls-cert %s", surfTLSCertPath)
		fmt.Fprintf(&b, " --tls-key %s", surfTLSKeyPath)
	} else {
		b.WriteString(" --insecure")
	}
	if opts.GatewayURL != "" {
		fmt.Fprintf(&b, " --gateway %s", shellEscape(opts.GatewayURL))
	}
	if opts.GatewayToken != "" {
		fmt.Fprintf(&b, " --gateway-token %s", shellEscape(opts.GatewayToken))
	}
	return b.String()
}

// shellEscape produces a single-quoted shell literal safe to embed in a
// command line. Internal single quotes use the standard '\” technique.
func shellEscape(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
