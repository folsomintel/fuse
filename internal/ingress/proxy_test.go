package ingress

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/folsomintel/fuse/internal/tunnel"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func mustCertPEM(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	pem, err := tunnel.CertPEM(cert)
	if err != nil {
		t.Fatal(err)
	}
	return pem
}

// rig is a whole fuse-proxy: routes, tunnel listener and admin api, on
// loopback, with its state in a directory the test can start a second one from.
type rig struct {
	proxy *Proxy
	admin *Client
	dir   string
}

func newRig(t *testing.T, dir string) *rig {
	t.Helper()
	cert, err := tunnel.LoadOrCreateCert(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := New(Config{
		StatePath: filepath.Join(dir, "routes.json"),
		// a wide range: the test does not own these ports, and allocation
		// skips whatever else on the machine holds one.
		PortMin: 42000, PortMax: 42999,
		BindHost:    "127.0.0.1",
		HoldTimeout: 5 * time.Second,
		Logger:      quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := tunnel.Listen(udp, cert, proxy.Authenticate, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = server.Serve(ctx) }()
	proxy.Start(server)

	web := httptest.NewServer(AdminHandler(proxy, AdminConfig{
		Token:         "admin-token",
		PublicHost:    "127.0.0.1",
		TunnelPort:    udp.LocalAddr().(*net.UDPAddr).Port,
		ServerCertPEM: mustCertPEM(t, cert),
	}))
	t.Cleanup(func() { web.Close(); cancel(); proxy.Close(); _ = udp.Close() })
	return &rig{proxy: proxy, admin: NewClient(web.URL, "admin-token"), dir: dir}
}

// guest is a service that answers every line with its own name, plus the
// sidecar that publishes it. the name is how a test tells which guest a public
// port led to.
func startGuest(t *testing.T, name string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					_, _ = io.WriteString(c, name+"\n")
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func (r *rig) publish(t *testing.T, owner string, guestPort int, adoptFrom string) orchestrator.IngressGrant {
	t.Helper()
	grant, err := r.admin.Publish(context.Background(), orchestrator.IngressRequest{
		Owner: owner, Token: "token-" + owner, AdoptFrom: adoptFrom,
		Ports: []orchestrator.IngressPort{{Name: "agent", GuestPort: guestPort}},
	})
	if err != nil {
		t.Fatalf("publish %s: %v", owner, err)
	}
	if len(grant.Routes) != 1 {
		t.Fatalf("publish %s: routes = %+v, want one", owner, grant.Routes)
	}
	return grant
}

func runSidecar(t *testing.T, grant orchestrator.IngressGrant, owner string, guestPort int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = tunnel.Run(ctx, tunnel.Config{
			ProxyAddr: grant.ProxyAddr, ServerCertPEM: grant.ServerCertPEM,
			Owner: owner, Token: "token-" + owner, Ports: []int{guestPort},
		}, quiet)
	}()
}

// ask connects to a public url, sends a line and returns the guest's answer.
func ask(t *testing.T, url string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", url, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, "who\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read from %s: %v", url, err)
	}
	return line[:len(line)-1]
}

func TestAClientReachesTheGuestThroughItsPublicPort(t *testing.T) {
	r := newRig(t, t.TempDir())
	port := startGuest(t, "vm-1")
	grant := r.publish(t, "vm-1", port, "")
	runSidecar(t, grant, "vm-1", port)

	if got := ask(t, grant.Routes[0].URL); got != "vm-1" {
		t.Fatalf("answered by %q, want vm-1", got)
	}
}

// the reason the proxy exists: a migrated vm takes over the url, and a client
// holding that url reaches the new guest without being told anything.
func TestAdoptMovesTheURLToTheNewGuest(t *testing.T) {
	r := newRig(t, t.TempDir())
	oldPort, newPort := startGuest(t, "old"), startGuest(t, "new")
	oldGrant := r.publish(t, "old", oldPort, "")
	runSidecar(t, oldGrant, "old", oldPort)
	url := oldGrant.Routes[0].URL
	if got := ask(t, url); got != "old" {
		t.Fatalf("before the migrate: answered by %q", got)
	}

	// a connection opened before the cutover belongs to the old guest and must
	// not be cut by it.
	held, err := net.Dial("tcp", url)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	heldReader := bufio.NewReader(held)
	_, _ = io.WriteString(held, "who\n")
	if line, _ := heldReader.ReadString('\n'); line != "old\n" {
		t.Fatalf("held connection answered %q", line)
	}

	// the order a migrate uses: the new vm is published and dials in under a
	// port of its own, and adopts only once it is known to work.
	fresh := r.publish(t, "new", newPort, "")
	runSidecar(t, fresh, "new", newPort)
	if fresh.Routes[0].URL == url {
		t.Fatal("the new vm was handed the old vm's port before adopting it")
	}
	if got := ask(t, fresh.Routes[0].URL); got != "new" {
		t.Fatalf("new vm's own port answered by %q", got)
	}

	adopted := r.publish(t, "new", newPort, "old")
	if adopted.Routes[0].URL != url {
		t.Fatalf("after adopt the url is %s, want the old vm's %s", adopted.Routes[0].URL, url)
	}
	if got := ask(t, url); got != "new" {
		t.Fatalf("after the migrate: answered by %q, want new", got)
	}
	_, _ = io.WriteString(held, "who\n")
	if line, _ := heldReader.ReadString('\n'); line != "old\n" {
		t.Fatalf("the connection that predates the cutover answered %q, want it still on old", line)
	}
	if routes, _ := r.proxy.Routes("old"); len(routes) != 0 {
		t.Fatalf("old still owns %+v", routes)
	}
	// the port the new vm gave up is closed, not leaked.
	if c, err := net.DialTimeout("tcp", fresh.Routes[0].URL, time.Second); err == nil {
		_ = c.Close()
		t.Fatalf("%s is still listening after the adopt replaced it", fresh.Routes[0].URL)
	}
}

// a url is only stable if it survives the proxy restarting.
func TestRoutesKeepTheirPortsAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	first := newRig(t, dir)
	port := startGuest(t, "vm-1")
	before := first.publish(t, "vm-1", port, "")
	first.proxy.Close()

	second := newRig(t, dir)
	after, err := second.admin.Lookup(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("lookup after restart: %v", err)
	}
	if after.Routes[0].URL != before.Routes[0].URL {
		t.Fatalf("url moved from %s to %s across a restart", before.Routes[0].URL, after.Routes[0].URL)
	}
	if after.ServerCertPEM != before.ServerCertPEM {
		t.Fatal("the tunnel certificate changed across a restart; every running guest pins the old one")
	}
	// the token survived too: the same guest can dial back in.
	runSidecar(t, after, "vm-1", port)
	if got := ask(t, after.Routes[0].URL); got != "vm-1" {
		t.Fatalf("after restart: answered by %q", got)
	}
}

func TestRemoveClosesThePortAndForgetsTheOwner(t *testing.T) {
	r := newRig(t, t.TempDir())
	grant := r.publish(t, "vm-1", startGuest(t, "vm-1"), "")
	if err := r.admin.Unpublish(context.Background(), "vm-1"); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", grant.Routes[0].URL, time.Second); err == nil {
		_ = c.Close()
		t.Fatal("the port is still open after the owner was removed")
	}
	if _, err := r.admin.Lookup(context.Background(), "vm-1"); err != orchestrator.ErrIngressNotFound {
		t.Fatalf("lookup of a removed owner = %v, want ErrIngressNotFound", err)
	}
	if err := r.admin.Unpublish(context.Background(), "vm-1"); err != orchestrator.ErrIngressNotFound {
		t.Fatalf("second unpublish = %v, want ErrIngressNotFound", err)
	}
	if r.proxy.Authenticate("vm-1", "token-vm-1") {
		t.Fatal("a removed owner's token still authenticates")
	}
}

// republishing is how a token is rotated; the url must not move when it does.
func TestRepublishKeepsThePortAndRotatesTheToken(t *testing.T) {
	r := newRig(t, t.TempDir())
	port := startGuest(t, "vm-1")
	first := r.publish(t, "vm-1", port, "")

	second, err := r.admin.Publish(context.Background(), orchestrator.IngressRequest{
		Owner: "vm-1", Token: "rotated",
		Ports: []orchestrator.IngressPort{{Name: "agent", GuestPort: port}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Routes[0].URL != first.Routes[0].URL {
		t.Fatalf("url moved from %s to %s on a republish", first.Routes[0].URL, second.Routes[0].URL)
	}
	if r.proxy.Authenticate("vm-1", "token-vm-1") || !r.proxy.Authenticate("vm-1", "rotated") {
		t.Fatal("the old token still works, or the new one does not")
	}
}

func TestAdminAPIRequiresTheToken(t *testing.T) {
	r := newRig(t, t.TempDir())
	stranger := NewClient(r.admin.BaseURL, "wrong")
	if _, err := stranger.Lookup(context.Background(), "vm-1"); err == nil || err == orchestrator.ErrIngressNotFound {
		t.Fatalf("lookup with the wrong token = %v, want a refusal", err)
	}
	res, err := http.Get(r.admin.BaseURL + "/healthz")
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("healthz without a token = %v, %v", res, err)
	}
	_ = res.Body.Close()
}

func TestPublishRejectsBadPorts(t *testing.T) {
	r := newRig(t, t.TempDir())
	for name, ports := range map[string][]PortSpec{
		"duplicate name": {{Name: "a", GuestPort: 80}, {Name: "a", GuestPort: 81}},
		"no name":        {{GuestPort: 80}},
		"port too large": {{Name: "a", GuestPort: 70000}},
	} {
		if _, err := r.proxy.Publish("vm-1", "t", ports, ""); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, ok := r.proxy.Routes("vm-1"); ok {
		t.Error("a rejected publish created the owner")
	}
}

func TestPortRangeExhaustionTakesNothingFromTheDonor(t *testing.T) {
	dir := t.TempDir()
	proxy, err := New(Config{StatePath: filepath.Join(dir, "routes.json"), PortMin: 42990, PortMax: 42990, BindHost: "127.0.0.1", Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	if _, err := proxy.Publish("old", "t", []PortSpec{{Name: "agent", GuestPort: 9550}}, ""); err != nil {
		t.Skipf("port 42990 is not free on this machine: %v", err)
	}
	// "agent" can be adopted, "extra" needs a port the range does not have.
	_, err = proxy.Publish("new", "t", []PortSpec{{Name: "agent", GuestPort: 9550}, {Name: "extra", GuestPort: 80}}, "old")
	if err == nil {
		t.Fatal("publish succeeded with no free port")
	}
	if routes, _ := proxy.Routes("old"); len(routes) != 1 || routes[0].PublicPort != 42990 {
		t.Fatalf("the failed publish changed the donor: %+v", routes)
	}
	if _, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(42990), time.Second); err != nil {
		t.Fatalf("the donor's port stopped listening: %v", err)
	}
}
