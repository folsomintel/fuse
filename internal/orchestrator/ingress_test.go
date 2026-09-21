package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/folsomintel/fuse/internal/egress"
	"github.com/folsomintel/fuse/internal/tunnel"
)

// fakeIngress is fuse-proxy as the fleet sees it: it hands out one url per
// (owner, route), lets an adopt move them, and records the order of calls.
type fakeIngress struct {
	mu         sync.Mutex
	routes     map[string]map[string]string // owner -> route name -> url
	tokens     map[string]string
	next       int
	calls      []string
	publishErr error
}

func newFakeIngress() *fakeIngress {
	return &fakeIngress{routes: map[string]map[string]string{}, tokens: map[string]string{}}
}

func (f *fakeIngress) grantLocked(owner string, ports []IngressPort) IngressGrant {
	grant := IngressGrant{ProxyAddr: "proxy.test:7443", ServerCertPEM: "pem"}
	for _, p := range ports {
		grant.Routes = append(grant.Routes, IngressRoute{Name: p.Name, GuestPort: p.GuestPort, URL: f.routes[owner][p.Name]})
	}
	return grant
}

func (f *fakeIngress) Publish(_ context.Context, req IngressRequest) (IngressGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.publishErr != nil {
		return IngressGrant{}, f.publishErr
	}
	f.calls = append(f.calls, strings.TrimSuffix("publish "+req.Owner+" adopt="+req.AdoptFrom, " adopt="))
	if f.routes[req.Owner] == nil {
		f.routes[req.Owner] = map[string]string{}
	}
	for _, p := range req.Ports {
		if url, ok := f.routes[req.AdoptFrom][p.Name]; ok {
			f.routes[req.Owner][p.Name] = url
			delete(f.routes[req.AdoptFrom], p.Name)
		} else if _, ok := f.routes[req.Owner][p.Name]; !ok {
			f.next++
			f.routes[req.Owner][p.Name] = fmt.Sprintf("proxy.test:%d", 20000+f.next)
		}
	}
	f.tokens[req.Owner] = req.Token
	return f.grantLocked(req.Owner, req.Ports), nil
}

func (f *fakeIngress) Lookup(_ context.Context, owner string) (IngressGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.routes[owner]; !ok {
		return IngressGrant{}, ErrIngressNotFound
	}
	return f.grantLocked(owner, []IngressPort{{Name: ingressAgentRoute, GuestPort: fusedGuestPort}}), nil
}

func (f *fakeIngress) Unpublish(_ context.Context, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "unpublish "+owner)
	if _, ok := f.routes[owner]; !ok {
		return ErrIngressNotFound
	}
	delete(f.routes, owner)
	delete(f.tokens, owner)
	return nil
}

func (f *fakeIngress) owners() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.routes))
	for owner := range f.routes {
		out = append(out, owner)
	}
	return out
}

func guestTunnelConfig(t *testing.T, provider *forkTestProvider, vmID string) tunnel.Config {
	t.Helper()
	env := provider.envs[vmID]
	if env == nil {
		t.Fatalf("no env for %s", vmID)
	}
	env.mu.Lock()
	raw := env.files[TunnelConfigGuestPath]
	env.mu.Unlock()
	if raw == nil {
		t.Fatalf("%s was never uploaded to %s", TunnelConfigGuestPath, vmID)
	}
	var cfg tunnel.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestProvision_publishesAndReportsTheStableURL(t *testing.T) {
	provider, proxy := newForkTestProvider(), newFakeIngress()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-", Ingress: proxy})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	info, _ := fm.GetVM(vmID)
	if info.URL != proxy.routes[vmID][ingressAgentRoute] || !strings.HasPrefix(info.URL, "proxy.test:") {
		t.Fatalf("reported url %q, want the proxy's %q", info.URL, proxy.routes[vmID][ingressAgentRoute])
	}
	cfg := guestTunnelConfig(t, provider, vmID)
	if cfg.Owner != vmID || cfg.Token == "" || cfg.Token != proxy.tokens[vmID] {
		t.Errorf("guest config owner %q token %q, want this vm's identity and the token the proxy was given", cfg.Owner, cfg.Token)
	}
	if cfg.ProxyAddr != "proxy.test:7443" || cfg.ServerCertPEM != "pem" || len(cfg.Ports) != 1 || cfg.Ports[0] != fusedGuestPort {
		t.Errorf("guest config = %+v", cfg)
	}
	// the orchestrator's own path to the guest does not go through the proxy.
	if rec, ok := fm.snapshotVMRecord(vmID); !ok || strings.HasPrefix(rec.URL, "proxy.test") {
		t.Errorf("persisted url %q, want the host's", rec.URL)
	}

	if err := fm.DestroyVM(context.Background(), vmID); err != nil {
		t.Fatal(err)
	}
	if owners := proxy.owners(); len(owners) != 0 {
		t.Errorf("owners after destroy = %v, want none", owners)
	}
}

func TestProvision_withoutAProxyIsUnchanged(t *testing.T) {
	provider := newForkTestProvider()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-"})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	info, _ := fm.GetVM(vmID)
	if info.URL != "http://"+vmID+".test" {
		t.Errorf("url = %q, want the provider's", info.URL)
	}
	if _, uploaded := provider.envs[vmID].files[TunnelConfigGuestPath]; uploaded {
		t.Error("a tunnel config was uploaded with no proxy configured")
	}
}

// its tap drops outbound traffic, so its sidecar could never dial out.
func TestProvision_proxyEgressIsNotPublished(t *testing.T) {
	provider, proxy := newForkTestProvider(), newFakeIngress()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-", Ingress: proxy})
	if fm.ingressSupported("", Spec{Egress: egress.Spec{Mode: egress.ModeProxy}}) {
		t.Fatal("a proxy-egress environment would be published")
	}
	if !fm.ingressSupported("", Spec{}) {
		t.Fatal("a direct-egress environment would not be published")
	}
}

func TestProvision_aPublishFailureFailsTheCreate(t *testing.T) {
	provider, proxy := newForkTestProvider(), newFakeIngress()
	proxy.publishErr = errors.New("proxy is down")
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-", Ingress: proxy})

	_, err := fm.ProvisionAndAssign(context.Background(), "task-1", Spec{}, []byte(`{"version":"1","services":{}}`), nil, BootOptions{})
	if err == nil || !strings.Contains(err.Error(), "proxy is down") {
		t.Fatalf("err = %v, want the publish failure", err)
	}
	if vms := fm.ListFleet(); len(vms) != 0 {
		t.Errorf("%d vms tracked, want none: a vm outlived a create that could not be published", len(vms))
	}
	if owners := proxy.owners(); len(owners) != 0 {
		t.Errorf("owners = %v, want none", owners)
	}
}

// a migrate keeps the url, and takes it from the source only at the very end.
func TestMigrateVM_adoptsTheSourcesURLLast(t *testing.T) {
	provider, proxy := newMigrateTestProvider(), newFakeIngress()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-", Ingress: proxy})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")
	before, _ := fm.GetVM(srcID)

	newID, err := fm.MigrateVM(context.Background(), srcID, MigrateOptions{})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	after, _ := fm.GetVM(newID)
	if after.URL != before.URL {
		t.Fatalf("url changed from %q to %q across the migrate", before.URL, after.URL)
	}
	if cfg := guestTunnelConfig(t, provider.forkTestProvider, newID); cfg.Owner != newID || cfg.Token != proxy.tokens[newID] {
		t.Errorf("migrated guest config = %+v, want an identity of its own", cfg)
	}
	want := []string{"publish " + srcID, "publish " + newID, "publish " + newID + " adopt=" + srcID, "unpublish " + srcID}
	if strings.Join(proxy.calls, "|") != strings.Join(want, "|") {
		t.Errorf("proxy calls = %v\nwant %v", proxy.calls, want)
	}
}

func TestMigrateVM_aFailedMigrateLeavesTheSourcesURLAlone(t *testing.T) {
	provider, proxy := newMigrateTestProvider(), newFakeIngress()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-", Ingress: proxy})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")
	before, _ := fm.GetVM(srcID)

	provider.failCreateFromCheckpoint = true
	if _, err := fm.MigrateVM(context.Background(), srcID, MigrateOptions{}); err == nil {
		t.Fatal("migrate succeeded")
	}
	if after, _ := fm.GetVM(srcID); after.URL != before.URL {
		t.Errorf("source url changed from %q to %q", before.URL, after.URL)
	}
	if owners := proxy.owners(); len(owners) != 1 || owners[0] != srcID {
		t.Errorf("owners = %v, want only the source", owners)
	}
}

// a fork is a new environment: it gets a url of its own and leaves the source's.
func TestForkEnvironment_getsItsOwnURL(t *testing.T) {
	provider, proxy := newForkTestProvider(), newFakeIngress()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-", Ingress: proxy})
	srcID := provisionSnapshotTestVM(t, fm, "task-1")

	forkID, err := fm.ForkEnvironment(context.Background(), srcID, ForkOptions{})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	src, _ := fm.GetVM(srcID)
	fork, _ := fm.GetVM(forkID)
	if fork.URL == src.URL || !strings.HasPrefix(fork.URL, "proxy.test:") {
		t.Errorf("fork url %q, source url %q: want two different proxy urls", fork.URL, src.URL)
	}
	// dev mode, no encryption key: the agent is otherwise left alone on a
	// fork, and the tunnel config still has to be replaced.
	if cfg := guestTunnelConfig(t, provider, forkID); cfg.Owner != forkID {
		t.Errorf("fork's guest config names %q, want %q", cfg.Owner, forkID)
	}
}
