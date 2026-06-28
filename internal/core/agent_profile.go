package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/folsomintel/fuse/internal/secrets"
)

// This file is the SINGLE home for all fused-specific knowledge. The core
// Boot/drain/provider code is generic: it uploads AgentSpec.Files, launches
// AgentSpec.Command (or honors structured fields a provider chooses to read),
// and runs AgentSpec.DrainCommand for graceful shutdown. The fused profile
// below reproduces the orchestrator's original behavior — the /fuse/* guest
// paths and the fused launch command line live here and nowhere else.

// fused guest paths. These follow the fused `/fuse/` convention (see PRD-08):
// the guest fc-agent mounts /fuse on tmpfs so secrets never reach persistent
// storage. They are profile details, not core defaults.
const (
	fuseManifestPath  = "/fuse/manifest.json"
	fuseSecretsPath   = "/fuse/secrets.json"
	fuseTLSCertPath   = "/fuse/tls/cert.pem"
	fuseTLSKeyPath    = "/fuse/tls/key.pem"
	fuseAuthTokenPath = "/fuse/auth-token"
)

// FuseSecretsPath is exported so fleet.reuploadSecrets can re-upload to the
// profile-declared path without hardcoding the literal.
const FuseSecretsPath = fuseSecretsPath

// fuseAgentBinaryPath is where the fused profile expects the daemon binary to
// live inside the guest. It must match the binary path referenced by the
// command line FusedAgentSpec assembles (and the destination a download-based
// provider writes to).
const fuseAgentBinaryPath = "/usr/local/bin/fused"

// DefaultFusedDrainCommand is the fused profile's graceful-stop invocation, run
// inside the guest via Environment.ExecStream on Drain.
//
// The fused agent traps SIGTERM and tears down its service DAG before exiting,
// so stopping it with a signal IS a full graceful drain. On the firecracker
// profile fused runs as a systemd unit, so `systemctl stop` delivers SIGTERM,
// waits for the clean exit, and (because the stop is clean) the unit's
// Restart=on-failure does NOT bring it back. The `pkill -TERM` fallback covers
// non-systemd guests, which also quiesce on SIGTERM.
//
// No trailing `|| true`: a genuine failure (neither a unit nor a process was
// stopped) propagates a non-zero exit so Drain records the error and keeps the
// VM Draining. A graceful-stop command that silently no-op'd (e.g. a missing
// helper hidden behind `|| true`) would defeat that.
const DefaultFusedDrainCommand = "systemctl stop fused 2>/dev/null || pkill -TERM fused"

// DefaultFusedManifest is the fused profile's default compiled manifest, used
// by the API when a caller omits an inline manifest. It encodes the fused
// manifest schema and is profile data, not a generic core default.
var DefaultFusedManifest = []byte(`{"version":"1","machine":{"workspace":"/workspace"},"services":{}}`)

// fusedCredentialFiles returns the fused profile's per-VM credential files
// (TLS cert, TLS key, auth token) keyed by guest path, or nil when creds is
// nil. Reused by FusedAgentSpec (fresh boot) and token rotation (re-upload).
func fusedCredentialFiles(creds *secrets.VMCredentials) map[string][]byte {
	if creds == nil {
		return nil
	}
	return map[string][]byte{
		fuseTLSCertPath:   creds.CertPEM,
		fuseTLSKeyPath:    creds.KeyPEM,
		fuseAuthTokenPath: []byte(creds.AuthToken),
	}
}

// FusedAgentSpec builds the fused profile: the full AgentSpec that reproduces
// the orchestrator's original boot behavior. Files carries the manifest, the
// secrets JSON (defaulting to "{}" when nil/empty), and — when creds is set —
// the TLS/auth credential files. Command is the fused launch line.
//
// NOTE on Command: the Command field is the generic launch line for providers
// that run a free-form shell command. The firecracker host agent IGNORES
// Command and instead reads structured fields off the frozen /start-surfd wire
// (manifest/secrets/TLS paths), sourcing them from its own path constants that
// mirror the consts above.
func FusedAgentSpec(manifest []byte, secretMap map[string]string, creds *secrets.VMCredentials, opts BootOptions) AgentSpec {
	files := map[string][]byte{
		fuseManifestPath: manifest,
	}

	// Default to an empty secret map so the guest always finds a valid
	// /fuse/secrets.json (this is the empty-map default logic, centralized
	// here in the profile).
	secretsMap := secretMap
	if secretsMap == nil {
		secretsMap = map[string]string{}
	}
	secretsJSON, _ := json.Marshal(secretsMap)
	files[fuseSecretsPath] = secretsJSON

	for path, data := range fusedCredentialFiles(creds) {
		files[path] = data
	}

	spec := AgentSpec{
		Files:        files,
		Command:      buildFusedCommand(creds, opts),
		Gateway:      opts.GatewayURL,
		GatewayToken: opts.GatewayToken,
		DrainCommand: DefaultFusedDrainCommand,
	}
	if creds != nil {
		spec.AuthToken = creds.AuthToken
	}
	return spec
}

// buildFusedCommand reconstructs the fused launch command line that the
// orchestrator originally produced. FUSE_CONTAINER_BIN=/bin/true is set
// because some sandbox images have no container runtime. When credentials are
// present the daemon runs with TLS + a file-backed auth token; otherwise it
// runs --insecure. Gateway values are shell-escaped to match prior behavior.
func buildFusedCommand(creds *secrets.VMCredentials, opts BootOptions) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "FUSE_CONTAINER_BIN=/bin/true %s", fuseAgentBinaryPath)
	b.WriteString(" --listen 0.0.0.0:3000")
	fmt.Fprintf(&b, " --manifest %s", fuseManifestPath)
	fmt.Fprintf(&b, " --secrets %s", fuseSecretsPath)
	if creds != nil {
		fmt.Fprintf(&b, " --auth-token-file %s", fuseAuthTokenPath)
		fmt.Fprintf(&b, " --tls-cert %s", fuseTLSCertPath)
		fmt.Fprintf(&b, " --tls-key %s", fuseTLSKeyPath)
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
