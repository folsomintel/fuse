package egress

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// the constants are a wire contract shared with the host agents, the api
// and four sdks, so they are pinned by literal here and not by symbol.
func TestWireValues(t *testing.T) {
	if ModeDirect != "direct" || ModeProxy != "proxy" {
		t.Fatalf("mode wire values changed: %q %q", ModeDirect, ModeProxy)
	}
	if ProtocolSOCKS5 != "socks5" || ProtocolHTTP != "http" {
		t.Fatalf("protocol wire values changed: %q %q", ProtocolSOCKS5, ProtocolHTTP)
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   Spec
		want Spec
	}{
		{"zero is direct", Spec{}, Spec{Mode: ModeDirect}},
		{"direct stays direct", Spec{Mode: ModeDirect}, Spec{Mode: ModeDirect}},
		{"proxy defaults to socks5", Spec{Mode: ModeProxy, Provider: "mock"}, Spec{Mode: ModeProxy, Provider: "mock", Protocol: ProtocolSOCKS5}},
		{"proxy keeps http", Spec{Mode: ModeProxy, Provider: "mock", Protocol: ProtocolHTTP}, Spec{Mode: ModeProxy, Provider: "mock", Protocol: ProtocolHTTP}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Normalize(); got != tc.want {
				t.Fatalf("Normalize(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		in      Spec
		wantErr string
	}{
		{"zero", Spec{}, ""},
		{"direct", Spec{Mode: ModeDirect}, ""},
		{"proxy socks5", Spec{Mode: ModeProxy, Provider: "mock"}, ""},
		{"proxy http", Spec{Mode: ModeProxy, Provider: "mock", Protocol: ProtocolHTTP}, ""},
		// a provider on a direct spec is the caller believing they asked
		// for proxying; it must not silently get direct egress.
		{"direct with provider", Spec{Mode: ModeDirect, Provider: "mock"}, `provider "mock" requires mode "proxy"`},
		{"zero mode with provider", Spec{Provider: "mock"}, `provider "mock" requires mode "proxy"`},
		{"direct with protocol", Spec{Protocol: ProtocolSOCKS5}, `protocol "socks5" requires mode "proxy"`},
		{"proxy without provider", Spec{Mode: ModeProxy}, `mode "proxy" requires a provider`},
		{"proxy unknown protocol", Spec{Mode: ModeProxy, Provider: "mock", Protocol: "ftp"}, `unknown protocol "ftp"`},
		{"unknown mode", Spec{Mode: "tunnel"}, `unknown mode "tunnel"`},
		{"mode is case sensitive", Spec{Mode: "Proxy", Provider: "mock"}, `unknown mode "Proxy"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate(%+v) = %v, want nil", tc.in, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate(%+v) = %v, want error containing %q", tc.in, err, tc.wantErr)
			}
		})
	}
}

// fakeProvider records what the registry asked of it and answers with
// whatever the test configured.
type fakeProvider struct {
	name         string
	endpoint     Endpoint
	provisionErr error
	healthErr    error
	provisioned  []Context
	destroyed    []Context
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Provision(_ context.Context, ec Context) (Endpoint, error) {
	f.provisioned = append(f.provisioned, ec)
	return f.endpoint, f.provisionErr
}

func (f *fakeProvider) Healthcheck(_ context.Context, _ Endpoint) error { return f.healthErr }

func (f *fakeProvider) Destroy(_ context.Context, ec Context) error {
	f.destroyed = append(f.destroyed, ec)
	return nil
}

func TestRegistryNamesAreSorted(t *testing.T) {
	r := NewRegistry(&fakeProvider{name: "warp"}, &fakeProvider{name: "mock"})
	got := r.Names()
	if len(got) != 2 || got[0] != "mock" || got[1] != "warp" {
		t.Fatalf("Names() = %v, want [mock warp]", got)
	}
}

func TestRegistryProvisionUnknownProvider(t *testing.T) {
	r := NewRegistry(&fakeProvider{name: "mock"})
	_, err := r.Provision(context.Background(), Spec{Mode: ModeProxy, Provider: "warp"}, Context{VMID: "vm-1"})
	var unknown *UnknownProviderError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want *UnknownProviderError", err)
	}
	if unknown.Name != "warp" || !strings.Contains(err.Error(), "registered: mock") {
		t.Fatalf("err = %v, want it to name warp and list mock", err)
	}

	// an orchestrator with nothing registered says so rather than listing
	// an empty set.
	_, err = NewRegistry().Provision(context.Background(), Spec{Mode: ModeProxy, Provider: "warp"}, Context{})
	if err == nil || !strings.Contains(err.Error(), "no egress providers are registered") {
		t.Fatalf("err = %v, want the empty-registry wording", err)
	}
}

func TestRegistryProvisionFillsEndpointAndContext(t *testing.T) {
	p := &fakeProvider{name: "mock", endpoint: Endpoint{URL: "socks5h://10.200.3.1:1080"}}
	r := NewRegistry(p)
	ec := Context{VMID: "vm-1", HostID: "h-1", ListenIP: "10.200.3.1", GuestIP: "10.200.3.2"}
	ep, err := r.Provision(context.Background(), Spec{Mode: ModeProxy, Provider: "mock"}, ec)
	if err != nil {
		t.Fatal(err)
	}
	// the provider only set the url; the registry stamps provider and
	// protocol so the endpoint can be health-checked and destroyed later
	// without the spec in hand.
	want := Endpoint{URL: "socks5h://10.200.3.1:1080", Protocol: ProtocolSOCKS5, Provider: "mock"}
	if ep != want {
		t.Fatalf("endpoint = %+v, want %+v", ep, want)
	}
	if len(p.provisioned) != 1 || p.provisioned[0].Protocol != ProtocolSOCKS5 || p.provisioned[0].ListenIP != "10.200.3.1" {
		t.Fatalf("provider saw context %+v, want the normalized protocol and the listen ip", p.provisioned)
	}
}

func TestRegistryProvisionRejectsDirectSpec(t *testing.T) {
	p := &fakeProvider{name: "mock", endpoint: Endpoint{URL: "socks5h://x"}}
	r := NewRegistry(p)
	if _, err := r.Provision(context.Background(), Spec{}, Context{}); err == nil {
		t.Fatal("direct spec provisioned, want an error")
	}
	if _, err := r.Provision(context.Background(), Spec{Mode: "bogus"}, Context{}); err == nil {
		t.Fatal("invalid spec provisioned, want an error")
	}
	if len(p.provisioned) != 0 {
		t.Fatalf("provider was called %d times for non-proxy specs", len(p.provisioned))
	}
}

func TestRegistryProvisionWithoutEndpointIsAFailure(t *testing.T) {
	p := &fakeProvider{name: "mock"}
	r := NewRegistry(p)
	_, err := r.Provision(context.Background(), Spec{Mode: ModeProxy, Provider: "mock"}, Context{VMID: "vm-1"})
	if err == nil || !strings.Contains(err.Error(), "returned no endpoint") {
		t.Fatalf("err = %v, want a no-endpoint failure", err)
	}
	// whatever the provider did allocate must be released: the vm's forward
	// rule is gone and there is nothing to route through.
	if len(p.destroyed) != 1 {
		t.Fatalf("Destroy called %d times, want 1", len(p.destroyed))
	}
}

func TestRegistryProvisionRejectsWrongProtocol(t *testing.T) {
	p := &fakeProvider{name: "warp", endpoint: Endpoint{URL: "socks5h://x", Protocol: ProtocolSOCKS5}}
	r := NewRegistry(p)
	_, err := r.Provision(context.Background(), Spec{Mode: ModeProxy, Provider: "warp", Protocol: ProtocolHTTP}, Context{VMID: "vm-1"})
	if err == nil || !strings.Contains(err.Error(), "answered a http request with a socks5 endpoint") {
		t.Fatalf("err = %v, want a protocol mismatch failure", err)
	}
	if len(p.destroyed) != 1 {
		t.Fatalf("Destroy called %d times, want 1", len(p.destroyed))
	}
}

func TestRegistryProvisionWrapsProviderError(t *testing.T) {
	p := &fakeProvider{name: "mock", provisionErr: errors.New("listen: address in use")}
	r := NewRegistry(p)
	_, err := r.Provision(context.Background(), Spec{Mode: ModeProxy, Provider: "mock"}, Context{VMID: "vm-1"})
	if err == nil || !errors.Is(err, p.provisionErr) || !strings.Contains(err.Error(), "vm-1") {
		t.Fatalf("err = %v, want the provider error wrapped with the vm id", err)
	}
}

func TestRegistryHealthcheck(t *testing.T) {
	p := &fakeProvider{name: "mock", healthErr: errors.New("dial: connection refused")}
	r := NewRegistry(p)
	err := r.Healthcheck(context.Background(), Endpoint{Provider: "mock"})
	if !errors.Is(err, p.healthErr) {
		t.Fatalf("err = %v, want the provider's verdict", err)
	}
	var unknown *UnknownProviderError
	if err := r.Healthcheck(context.Background(), Endpoint{Provider: "warp"}); !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want *UnknownProviderError", err)
	}
}

func TestRegistryDestroy(t *testing.T) {
	p := &fakeProvider{name: "mock"}
	r := NewRegistry(p)
	ec := Context{VMID: "vm-1"}
	if err := r.Destroy(context.Background(), "mock", ec); err != nil {
		t.Fatal(err)
	}
	if len(p.destroyed) != 1 || p.destroyed[0].VMID != "vm-1" {
		t.Fatalf("Destroy delegated %+v, want vm-1 once", p.destroyed)
	}
	// unregistered is a no-op, never an error: this runs on cleanup paths
	// that must not fail.
	if err := r.Destroy(context.Background(), "warp", ec); err != nil {
		t.Fatalf("Destroy of unknown provider = %v, want nil", err)
	}
}
