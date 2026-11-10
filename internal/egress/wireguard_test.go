package egress

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

const wgPrivateKey = "yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk="

const wgConf = `[Interface]
PrivateKey = ` + wgPrivateKey + `
Address = 10.64.0.2/32
DNS = 10.64.0.1

[Peer]
PublicKey = xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=
Endpoint = 203.0.113.7:51820
AllowedIPs = 0.0.0.0/0
`

func wgSecret(conf string) string { return base64.StdEncoding.EncodeToString([]byte(conf)) }

func wgContext(host HostAgent, vmID, hostID string) Context {
	return Context{VMID: vmID, HostID: hostID, ListenIP: "10.200.3.1", GuestIP: "10.200.3.2",
		Protocol: ProtocolSOCKS5, Host: host, Credential: wgSecret(wgConf)}
}

func TestWireGuardProvisionPassesTheSecretThroughConfigOnly(t *testing.T) {
	host := &fakeHostAgent{endpoint: Endpoint{URL: "socks5h://10.200.3.1:1080", Protocol: ProtocolSOCKS5}}
	w := NewWireGuard()
	ep, err := w.Provision(context.Background(), wgContext(host, "vm-1", "h-1"))
	if err != nil {
		t.Fatal(err)
	}
	if ep.Provider != WireGuardName || ep.URL != "socks5h://10.200.3.1:1080" {
		t.Fatalf("endpoint = %+v", ep)
	}
	req := host.provisioned[0]
	if req.Provider != WireGuardName || req.ListenIP != "10.200.3.1" || req.Protocol != ProtocolSOCKS5 {
		t.Fatalf("host request = %+v", req)
	}
	if len(req.Config) != 1 || req.Config["WG_CONF"] != wgSecret(wgConf) {
		t.Fatalf("config = %v", req.Config)
	}
}

func TestWireGuardRefusesMissingOrMalformedSecretsBeforeTheHost(t *testing.T) {
	for name, secret := range map[string]string{
		"missing":      "",
		"not base64":   "[Interface]\nPrivateKey = x",
		"not a config": wgSecret("hello"),
		"no peer":      wgSecret("[Interface]\nPrivateKey = x\n"),
		"whitespace":   "   ",
	} {
		t.Run(name, func(t *testing.T) {
			host := &fakeHostAgent{}
			ec := wgContext(host, "vm-1", "h-1")
			ec.Credential = secret
			if _, err := NewWireGuard().Provision(context.Background(), ec); err == nil {
				t.Fatal("provisioned")
			}
			if len(host.provisioned) != 0 {
				t.Fatal("a bad secret reached the host")
			}
		})
	}
}

func TestDecodeWireGuardConfigToleratesWrappedBase64(t *testing.T) {
	enc := wgSecret(wgConf)
	wrapped := enc[:20] + "\n" + enc[20:40] + "\n" + enc[40:] + "\n"
	conf, err := DecodeWireGuardConfig(wrapped)
	if err != nil || conf != wgConf {
		t.Fatalf("conf=%q err=%v", conf, err)
	}
}

func TestWireGuardRefusesTheSameConfigOnASecondHost(t *testing.T) {
	host := &fakeHostAgent{endpoint: Endpoint{URL: "socks5h://10.200.3.1:1080", Protocol: ProtocolSOCKS5}}
	w := NewWireGuard()
	if _, err := w.Provision(context.Background(), wgContext(host, "vm-1", "h-1")); err != nil {
		t.Fatal(err)
	}
	// same host shares the bridge.
	if _, err := w.Provision(context.Background(), wgContext(host, "vm-2", "h-1")); err != nil {
		t.Fatalf("same host refused: %v", err)
	}
	_, err := w.Provision(context.Background(), wgContext(host, "vm-3", "h-2"))
	if err == nil || !strings.Contains(err.Error(), "host h-1") {
		t.Fatalf("second host err = %v", err)
	}
	if len(host.provisioned) != 2 {
		t.Fatalf("host asked %d times, want 2", len(host.provisioned))
	}
	// once every vm on h-1 is gone the key is free to move.
	_ = w.Destroy(context.Background(), wgContext(host, "vm-1", "h-1"))
	_ = w.Destroy(context.Background(), wgContext(host, "vm-2", "h-1"))
	if _, err := w.Provision(context.Background(), wgContext(host, "vm-3", "h-2")); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestWireGuardFailedProvisionHoldsNoReservation(t *testing.T) {
	host := &fakeHostAgent{provisionErr: errors.New("host down")}
	w := NewWireGuard()
	if _, err := w.Provision(context.Background(), wgContext(host, "vm-1", "h-1")); err == nil {
		t.Fatal("provisioned")
	}
	host.provisionErr = nil
	host.endpoint = Endpoint{URL: "socks5h://10.200.3.1:1080", Protocol: ProtocolSOCKS5}
	if _, err := w.Provision(context.Background(), wgContext(host, "vm-2", "h-2")); err != nil {
		t.Fatalf("a failed provision left a reservation: %v", err)
	}
}

func TestWireGuardRedactsTheSecretAndKeysFromHostErrors(t *testing.T) {
	host := &fakeHostAgent{provisionErr: errors.New("bad config " + wgSecret(wgConf) + " key " + wgPrivateKey)}
	_, err := NewWireGuard().Provision(context.Background(), wgContext(host, "vm-1", "h-1"))
	if err == nil {
		t.Fatal("provisioned")
	}
	if strings.Contains(err.Error(), wgPrivateKey) || strings.Contains(err.Error(), wgSecret(wgConf)) {
		t.Fatalf("leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("not redacted: %v", err)
	}
}

func TestWireGuardNeedsAHost(t *testing.T) {
	ec := wgContext(nil, "vm-1", "h-1")
	ec.Host = nil
	if _, err := NewWireGuard().Provision(context.Background(), ec); err == nil {
		t.Fatal("provisioned without a host")
	}
	if err := NewWireGuard().Destroy(context.Background(), ec); err != nil {
		t.Fatalf("destroy without a host: %v", err)
	}
}

func TestProvidersWithoutACredentialRefuseOne(t *testing.T) {
	host := &fakeHostAgent{endpoint: Endpoint{URL: "socks5h://10.200.3.1:1080", Protocol: ProtocolSOCKS5}}
	ec := Context{VMID: "vm-1", ListenIP: "10.200.3.1", Protocol: ProtocolSOCKS5, Host: host, Credential: "x"}
	if _, err := warpZeroTrust().Provision(context.Background(), ec); err == nil {
		t.Fatal("warp accepted a secret")
	}
	ec.ListenIP = "127.0.0.1"
	m := NewMock()
	m.AllowLoopback = true
	if _, err := m.Provision(context.Background(), ec); err == nil {
		t.Fatal("mock accepted a secret")
	}
}

func TestSpecRejectsASecretOnDirect(t *testing.T) {
	if err := (Spec{Mode: ModeDirect, Secret: "WG"}).Validate(); err == nil {
		t.Fatal("direct with a secret validated")
	}
	if err := (Spec{Mode: ModeProxy, Provider: WireGuardName, Secret: "WG"}).Validate(); err != nil {
		t.Fatal(err)
	}
}
