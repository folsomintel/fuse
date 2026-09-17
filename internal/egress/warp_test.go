package egress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeHostAgent records the egress wire calls a provider makes and answers
// with whatever the test configured.
type fakeHostAgent struct {
	provisioned  []HostRequest
	destroyed    []string
	endpoint     Endpoint
	provisionErr error
	destroyErr   error
}

func (h *fakeHostAgent) ProvisionEgress(_ context.Context, _ string, req HostRequest) (Endpoint, error) {
	h.provisioned = append(h.provisioned, req)
	return h.endpoint, h.provisionErr
}

func (h *fakeHostAgent) DestroyEgress(_ context.Context, vmID string) error {
	h.destroyed = append(h.destroyed, vmID)
	return h.destroyErr
}

const warpSecret = "svc-secret-0123456789abcdef"

func warpZeroTrust() *Warp {
	return NewWarp(WarpConfig{Organization: "acme", AuthClientID: "client-id-0123456789", AuthClientSecret: warpSecret})
}

func TestWarpConfigEnabled(t *testing.T) {
	if (WarpConfig{}).Enabled() {
		t.Fatal("empty config reported enabled")
	}
	if !(WarpConfig{License: "k"}).Enabled() {
		t.Fatal("license alone should enable")
	}
	if (WarpConfig{Organization: "acme"}).Enabled() {
		t.Fatal("organization without a service token reported enabled")
	}
	if !(WarpConfig{Organization: "acme", AuthClientID: "id", AuthClientSecret: "s"}).Enabled() {
		t.Fatal("a full service token should enable")
	}
}

func TestWarpProvisionDrivesTheHostWire(t *testing.T) {
	host := &fakeHostAgent{endpoint: Endpoint{URL: "socks5h://10.200.3.1:1080", Protocol: ProtocolSOCKS5}}
	w := warpZeroTrust()
	ec := Context{VMID: "vm-1", HostID: "h-1", ListenIP: "10.200.3.1", GuestIP: "10.200.3.2", Protocol: ProtocolSOCKS5, Host: host}
	ep, err := w.Provision(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Provider != WarpName || ep.URL != "socks5h://10.200.3.1:1080" || ep.Protocol != ProtocolSOCKS5 {
		t.Fatalf("endpoint = %+v", ep)
	}
	if len(host.provisioned) != 1 {
		t.Fatalf("host asked %d times, want 1", len(host.provisioned))
	}
	req := host.provisioned[0]
	if req.Provider != WarpName || req.ListenIP != "10.200.3.1" || req.GuestIP != "10.200.3.2" || req.Protocol != ProtocolSOCKS5 {
		t.Fatalf("host request = %+v", req)
	}
	// the credential travels in config, and only there.
	if req.Config["WARP_ORG"] != "acme" || req.Config["WARP_CLIENT_SECRET"] != warpSecret || req.Config["WARP_CLIENT_ID"] != "client-id-0123456789" {
		t.Fatalf("config = %+v, want the service token", req.Config)
	}
	if _, present := req.Config["WARP_LICENSE"]; present {
		t.Fatal("a zero trust config must not carry a license key")
	}
}

func TestWarpProvisionRefusesWithoutHostOrAddress(t *testing.T) {
	w := warpZeroTrust()
	if _, err := w.Provision(context.Background(), Context{VMID: "vm-1", ListenIP: "10.200.3.1"}); err == nil || !strings.Contains(err.Error(), "no egress wire") {
		t.Fatalf("err = %v, want a no-host refusal", err)
	}
	host := &fakeHostAgent{}
	if _, err := w.Provision(context.Background(), Context{VMID: "vm-1", Host: host}); err == nil || !strings.Contains(err.Error(), "no tap address") {
		t.Fatalf("err = %v, want a no-address refusal", err)
	}
	if len(host.provisioned) != 0 {
		t.Fatal("host was asked despite a refused context")
	}
}

func TestWarpCredentialNeverAppearsInAnError(t *testing.T) {
	// a host agent that echoes the request back is the worst case: the
	// secret is in the error body verbatim.
	host := &fakeHostAgent{provisionErr: fmt.Errorf("http 400: bad config %s / %s", warpSecret, "client-id-0123456789")}
	w := warpZeroTrust()
	ec := Context{VMID: "vm-1", ListenIP: "10.200.3.1", Protocol: ProtocolSOCKS5, Host: host}
	_, err := w.Provision(context.Background(), ec)
	if err == nil {
		t.Fatal("expected the host's refusal")
	}
	if strings.Contains(err.Error(), warpSecret) || strings.Contains(err.Error(), "client-id-0123456789") {
		t.Fatalf("credential leaked into the error: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") || !strings.Contains(err.Error(), "http 400") {
		t.Fatalf("err = %v, want the host's reason with the credential redacted", err)
	}

	license := NewWarp(WarpConfig{License: "warp-plus-license-key-xyz"})
	host.destroyErr = errors.New("release failed for warp-plus-license-key-xyz")
	if err := license.Destroy(context.Background(), ec); err == nil || strings.Contains(err.Error(), "warp-plus-license-key-xyz") {
		t.Fatalf("destroy err = %v, want the license redacted", err)
	}
}

func TestWarpDestroyIsANoOpWithoutAHost(t *testing.T) {
	host := &fakeHostAgent{}
	w := warpZeroTrust()
	if err := w.Destroy(context.Background(), Context{VMID: "vm-1"}); err != nil {
		t.Fatalf("destroy without host = %v, want nil", err)
	}
	if err := w.Destroy(context.Background(), Context{VMID: "vm-1", Host: host}); err != nil {
		t.Fatal(err)
	}
	if len(host.destroyed) != 1 || host.destroyed[0] != "vm-1" {
		t.Fatalf("host destroyed %v, want [vm-1]", host.destroyed)
	}
}

func TestWarpThroughRegistry(t *testing.T) {
	host := &fakeHostAgent{endpoint: Endpoint{URL: "http://10.200.3.1:1080", Protocol: ProtocolHTTP}}
	r := NewRegistry(NewWarp(WarpConfig{License: "warp-plus-license-key-xyz", ProxyPort: 40000}))
	ec := Context{VMID: "vm-1", ListenIP: "10.200.3.1", GuestIP: "10.200.3.2", Host: host}
	ep, err := r.Provision(context.Background(), Spec{Mode: ModeProxy, Provider: WarpName, Protocol: ProtocolHTTP}, ec)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Protocol != ProtocolHTTP || ep.Provider != WarpName {
		t.Fatalf("endpoint = %+v", ep)
	}
	if got := host.provisioned[0].Config; got["WARP_LICENSE"] != "warp-plus-license-key-xyz" || got["WARP_PROXY_PORT"] != "40000" {
		t.Fatalf("config = %+v", got)
	}
	if err := r.Destroy(context.Background(), WarpName, ec); err != nil {
		t.Fatal(err)
	}
	if len(host.destroyed) != 1 {
		t.Fatalf("host destroyed %v", host.destroyed)
	}
}
