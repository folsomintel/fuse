package egress

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// WireGuardName is the provider name a caller uses to ask for wireguard.
const WireGuardName = "wireguard"

// wireGuardConfigKey is the HostRequest.Config key the host agent's
// wireguard backend reads: the base64 wg-quick config, passed through as is.
const wireGuardConfigKey = "WG_CONF"

// WireGuard is the control-plane half of the wireguard backend. unlike warp
// it holds no credential of its own: each environment brings one, as a
// base64 wg-quick config named by egress.secret, and the host bridges it to
// a socks5/http proxy on the vm's tap (wireproxy in a netns, one per config).
//
// a wireguard key can be connected from one place at a time: two clients on
// one key make the server flap between their endpoints. the host shares one
// bridge between every vm on it that uses the same config, so the only way
// to break that is the same config on two hosts, which this refuses. the
// record is in memory, so it starts empty after an orchestrator restart;
// it is a guardrail, not a lock.
type WireGuard struct {
	mu sync.Mutex
	// live maps a config fingerprint to the host holding it and the vms on
	// that host that use it.
	live map[string]*wireGuardBridge
}

type wireGuardBridge struct {
	hostID string
	vms    map[string]struct{}
}

// NewWireGuard returns a wireguard provider with nothing live.
func NewWireGuard() *WireGuard {
	return &WireGuard{live: map[string]*wireGuardBridge{}}
}

// Name implements Provider.
func (w *WireGuard) Name() string { return WireGuardName }

// DecodeWireGuardConfig decodes and sanity-checks the egress secret: base64
// of a wg-quick config with an [Interface] and at least one [Peer]. the
// host agent applies the strict key allowlist; this only catches the
// mistakes worth failing before a vm is touched (a raw config pasted
// without encoding, an empty or truncated value).
func DecodeWireGuardConfig(secret string) (string, error) {
	compact := strings.Join(strings.Fields(secret), "")
	if compact == "" {
		return "", errors.New("the egress secret is empty")
	}
	raw, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return "", errors.New("the egress secret is not base64; encode the wg-quick config with `base64 -w0 wg0.conf`")
	}
	conf := string(raw)
	if !utf8.ValidString(conf) {
		return "", errors.New("the decoded egress secret is not text")
	}
	lower := strings.ToLower(conf)
	if !strings.Contains(lower, "[interface]") || !strings.Contains(lower, "[peer]") {
		return "", errors.New("the decoded egress secret is not a wg-quick config (want an [Interface] and a [Peer] section)")
	}
	return conf, nil
}

func wireGuardFingerprint(conf string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(conf)))
	return hex.EncodeToString(sum[:])[:12]
}

// Provision implements Provider.
func (w *WireGuard) Provision(ctx context.Context, ec Context) (Endpoint, error) {
	if ec.Host == nil {
		return Endpoint{}, errors.New("wireguard: the vm's provider exposes no egress wire; the backend runs on a firecracker host")
	}
	if ec.ListenIP == "" {
		return Endpoint{}, errors.New("wireguard: the vm reported no tap address to publish the proxy on")
	}
	if ec.Credential == "" {
		return Endpoint{}, errors.New("wireguard: needs egress.secret naming a secret that holds a base64 wg-quick config")
	}
	conf, err := DecodeWireGuardConfig(ec.Credential)
	if err != nil {
		return Endpoint{}, fmt.Errorf("wireguard: %w", err)
	}
	fp := wireGuardFingerprint(conf)
	if err := w.reserve(fp, ec.HostID, ec.VMID); err != nil {
		return Endpoint{}, err
	}

	req := HostRequest{
		Provider: WireGuardName,
		Protocol: ec.Protocol,
		ListenIP: ec.ListenIP,
		GuestIP:  ec.GuestIP,
		Config:   map[string]string{wireGuardConfigKey: ec.Credential},
	}
	ep, err := ec.Host.ProvisionEgress(ctx, ec.VMID, req)
	if err != nil {
		w.forget(ec.VMID)
		return Endpoint{}, redactWireGuard(err, ec.Credential, conf)
	}
	ep.Provider = WireGuardName
	if ep.Protocol == "" {
		ep.Protocol = ec.Protocol
	}
	return ep, nil
}

// reserve records vmID as a user of the config on hostID, refusing when
// the same config is live on a different host.
func (w *WireGuard) reserve(fp, hostID, vmID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	b := w.live[fp]
	if b != nil && len(b.vms) > 0 && b.hostID != hostID {
		vms := make([]string, 0, len(b.vms))
		for id := range b.vms {
			vms = append(vms, id)
		}
		sort.Strings(vms)
		return fmt.Errorf("wireguard: this config is already connected from host %s (used by %s); a wireguard key can only be connected from one host at a time, so pin the environment to that host or use a different key",
			b.hostID, strings.Join(vms, ", "))
	}
	if b == nil || len(b.vms) == 0 {
		b = &wireGuardBridge{hostID: hostID, vms: map[string]struct{}{}}
		w.live[fp] = b
	}
	b.vms[vmID] = struct{}{}
	return nil
}

// forget drops vmID from whichever config it was using.
func (w *WireGuard) forget(vmID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for fp, b := range w.live {
		delete(b.vms, vmID)
		if len(b.vms) == 0 {
			delete(w.live, fp)
		}
	}
}

// Healthcheck implements Provider. it proves the bridge answers; the host
// refuses to report a bridge up until the tunnel has completed a handshake.
func (w *WireGuard) Healthcheck(ctx context.Context, ep Endpoint) error {
	return ProbeEndpoint(ctx, ep)
}

// Destroy implements Provider. the record is dropped even when the host
// call fails: a vm being torn down no longer uses the config either way.
func (w *WireGuard) Destroy(ctx context.Context, ec Context) error {
	w.forget(ec.VMID)
	if ec.Host == nil {
		return nil
	}
	if err := ec.Host.DestroyEgress(ctx, ec.VMID); err != nil {
		return fmt.Errorf("wireguard: %w", err)
	}
	return nil
}

// redactWireGuard strips the secret and its keys out of a host error. the
// fleet also redacts every secret value, but a host echoing the decoded
// config back would carry the private key in a form that is not the secret
// value itself.
func redactWireGuard(err error, secret, conf string) error {
	msg := err.Error()
	values := []string{strings.TrimSpace(secret)}
	for _, line := range strings.Split(conf, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "privatekey", "presharedkey":
			values = append(values, strings.TrimSpace(value))
		}
	}
	for _, v := range values {
		if len(v) >= 8 {
			msg = strings.ReplaceAll(msg, v, "[REDACTED]")
		}
	}
	return fmt.Errorf("wireguard: %s", msg)
}
