package tunnel

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// otherCertPEM is a valid certificate that is not the server's, so a guest
// carrying it is rejected for the right reason rather than for being malformed.
func otherCertPEM(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cert, err := LoadOrCreateCert(filepath.Join(dir, "other.pem"), filepath.Join(dir, "other.key"))
	if err != nil {
		t.Fatal(err)
	}
	pem, err := CertPEM(cert)
	if err != nil {
		t.Fatal(err)
	}
	return pem
}

// echoServer is the guest-side service: it echoes lines.
func echoServer(t *testing.T) int {
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
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// recorder is a Metrics that counts what the server reported.
type recorder struct {
	mu         sync.Mutex
	handshakes map[string]int
	connected  int
	replaced   int
	probes     map[bool]int
	migrations int
}

func (r *recorder) Handshake(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handshakes[result]++
}

func (r *recorder) GuestConnected(_ string, replaced bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connected++
	if replaced {
		r.replaced++
	}
}

func (r *recorder) GuestsConnected(int) {}

func (r *recorder) Probe(alive bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probes[alive]++
}

func (r *recorder) PathMigrated(string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.migrations++
}

type harness struct {
	srv    *Server
	hellos atomic.Int32
	cfg    Config
	rec    *recorder
	token  atomic.Value // the token the authenticator accepts
}

func startServer(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	cert, err := LoadOrCreateCert(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{rec: &recorder{handshakes: map[string]int{}, probes: map[bool]int{}}}
	h.token.Store("secret")
	h.srv, err = Listen(udp, cert, func(owner, token string) bool {
		h.hellos.Add(1)
		return owner == "vm-1" && token == h.token.Load().(string)
	}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	h.srv.SetMetrics(h.rec)
	h.srv.pathCheck = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = udp.Close() })
	go func() { _ = h.srv.Serve(ctx) }()
	certPEM, err := CertPEM(cert)
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = Config{
		ProxyAddr:     h.srv.Addr().String(),
		ServerCertPEM: certPEM,
		Owner:         "vm-1",
		Token:         "secret",
	}
	return h
}

func runSidecar(t *testing.T, cfg Config) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = Run(ctx, cfg, quiet); close(done) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

func open(t *testing.T, srv *Server, port int, wait time.Duration) (net.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return srv.Open(ctx, "vm-1", port)
}

func roundTrip(t *testing.T, c net.Conn, r *bufio.Reader, line string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, line+"\n"); err != nil {
		t.Fatalf("write %q: %v", line, err)
	}
	got, err := r.ReadString('\n')
	if err != nil || got != line+"\n" {
		t.Fatalf("echo of %q = %q, %v", line, got, err)
	}
}

func TestStreamsReachTheGuestPort(t *testing.T) {
	h := startServer(t)
	h.cfg.Ports = []int{echoServer(t)}
	runSidecar(t, h.cfg)

	c, err := open(t, h.srv, h.cfg.Ports[0], 5*time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()
	roundTrip(t, c, bufio.NewReader(c), "hello")
}

// the sidecar, not the proxy, decides what is reachable inside the guest.
func TestSidecarRefusesAPortThatIsNotPublished(t *testing.T) {
	h := startServer(t)
	unlisted := echoServer(t)
	h.cfg.Ports = []int{echoServer(t)}
	runSidecar(t, h.cfg)

	if _, err := open(t, h.srv, unlisted, 5*time.Second); err == nil {
		t.Fatal("a stream to an unpublished port was accepted")
	}
}

func TestWrongTokenAndWrongPinNeverRegister(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"token": func(c *Config) { c.Token = "wrong" },
		"pin":   func(c *Config) { c.ServerCertPEM = otherCertPEM(t) },
	} {
		t.Run(name, func(t *testing.T) {
			h := startServer(t)
			mutate(&h.cfg)
			runSidecar(t, h.cfg)
			time.Sleep(500 * time.Millisecond)
			if h.srv.Connected("vm-1") {
				t.Fatal("guest registered")
			}
		})
	}
}

// a client that connects while no guest is there is held, not refused. this is
// the window between a migrate repointing a route and the new guest dialing in.
func TestOpenWaitsForTheGuest(t *testing.T) {
	h := startServer(t)
	h.cfg.Ports = []int{echoServer(t)}

	var wg sync.WaitGroup
	wg.Add(1)
	var c net.Conn
	var err error
	go func() {
		defer wg.Done()
		c, err = open(t, h.srv, h.cfg.Ports[0], 10*time.Second)
	}()
	time.Sleep(300 * time.Millisecond)
	runSidecar(t, h.cfg)
	wg.Wait()
	if err != nil {
		t.Fatalf("open held for the guest, then failed: %v", err)
	}
	defer c.Close()
	roundTrip(t, c, bufio.NewReader(c), "held")

	if _, err := h.srv.Open(canceled(), "nobody", 1); err != ErrNoTunnel {
		t.Fatalf("open for an absent guest = %v, want ErrNoTunnel", err)
	}
}

func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// a byte-copied disk boots with its source's identity. it must not take over a
// tunnel whose guest is alive, and must take over one whose guest is gone.
func TestASecondGuestOnlyReplacesADeadOne(t *testing.T) {
	h := startServer(t)
	h.cfg.Ports = []int{echoServer(t)}
	stopFirst := runSidecar(t, h.cfg)
	c, err := open(t, h.srv, h.cfg.Ports[0], 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := bufio.NewReader(c)
	roundTrip(t, c, r, "before the clone")

	runSidecar(t, h.cfg) // the clone
	time.Sleep(time.Second)
	roundTrip(t, c, r, "the clone did not evict the live guest")

	stopFirst()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if c2, err := open(t, h.srv, h.cfg.Ports[0], time.Second); err == nil {
			defer c2.Close()
			roundTrip(t, c2, bufio.NewReader(c2), "the clone took over a dead tunnel")
			h.rec.mu.Lock()
			defer h.rec.mu.Unlock()
			// the clone's first try found the original alive; whether its
			// retry probed a dead guest or found the slot already empty
			// depends on how fast the original's close reached the proxy.
			if h.rec.probes[true] < 1 || h.rec.handshakes["duplicate"] < 1 || h.rec.connected != 2 {
				t.Fatalf("metrics = %+v, want an alive probe, a duplicate and two connections", h.rec)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the second guest never took over after the first stopped")
		}
	}
}

// relay forwards udp between the sidecar and the proxy, and can swap the socket
// it uses toward the proxy. to the proxy that is exactly what a guest resumed
// on another host looks like: same connection id, new source address.
type relay struct {
	front  net.PacketConn
	server net.Addr
	mu     sync.Mutex
	back   net.PacketConn
	client net.Addr
	// blocked drops every packet both ways, which to the proxy looks like a
	// paused guest.
	blocked atomic.Bool
}

func newRelay(t *testing.T, server net.Addr) *relay {
	t.Helper()
	front, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &relay{front: front, server: server}
	r.swap(t)
	t.Cleanup(func() { _ = front.Close(); _ = r.back.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, from, err := front.ReadFrom(buf)
			if err != nil {
				return
			}
			r.mu.Lock()
			r.client = from
			back := r.back
			r.mu.Unlock()
			if !r.blocked.Load() {
				_, _ = back.WriteTo(buf[:n], server)
			}
		}
	}()
	return r
}

// swap moves the relay to a new source address and returns the old one's.
func (r *relay) swap(t *testing.T) {
	t.Helper()
	back, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	old := r.back
	r.back = back
	r.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, _, err := back.ReadFrom(buf)
			if err != nil {
				return
			}
			r.mu.Lock()
			client := r.client
			r.mu.Unlock()
			if client != nil && !r.blocked.Load() {
				_, _ = r.front.WriteTo(buf[:n], client)
			}
		}
	}()
}

// the property the whole design rests on: the guest's address changes under an
// open stream and the stream carries on, on the same connection.
func TestAnOpenStreamSurvivesTheGuestChangingAddress(t *testing.T) {
	// after a swap the proxy keeps writing to the old address until the guest
	// sends from the new one. a real guest does that on its keepalive; at the
	// production 15s that is longer than roundTrip's deadline, and the test
	// passed only when a delayed ack happened to cross the swap. a cleanup
	// registered before runSidecar's, so it runs after the sidecar stopped.
	old := keepAlive
	t.Cleanup(func() { keepAlive = old })
	keepAlive = 200 * time.Millisecond

	h := startServer(t)
	h.cfg.Ports = []int{echoServer(t)}
	rl := newRelay(t, h.srv.Addr())
	h.cfg.ProxyAddr = rl.front.LocalAddr().String()
	runSidecar(t, h.cfg)

	c, err := open(t, h.srv, h.cfg.Ports[0], 5*time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()
	r := bufio.NewReader(c)
	roundTrip(t, c, r, "before the move")

	for i := 0; i < 3; i++ {
		rl.swap(t)
		roundTrip(t, c, r, "after the move")
	}
	if n := h.hellos.Load(); n != 1 {
		t.Fatalf("%d handshakes, want 1: the guest reconnected instead of migrating", n)
	}
	// the roundtrip after each swap proved the proxy took the new path, so
	// the next check of the address sees every move.
	time.Sleep(5 * h.srv.pathCheck)
	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	if h.rec.migrations < 1 || h.rec.connected != 1 {
		t.Fatalf("%d path migrations and %d connections, want at least 1 and exactly 1", h.rec.migrations, h.rec.connected)
	}
}

// downloadServer is a guest-side service that sends size bytes of a known
// pattern to every client, as fast as the client reads them.
func downloadServer(t *testing.T, size int) int {
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
				buf := make([]byte, 32<<10)
				for sent := 0; sent < size; sent += len(buf) {
					for i := range buf {
						buf[i] = byte((sent + i) % 251)
					}
					if _, err := c.Write(buf[:min(len(buf), size-sent)]); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// readPattern reads n bytes from r starting at offset and checks they follow
// downloadServer's pattern.
func readPattern(t *testing.T, r io.Reader, offset, n int) {
	t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read %d bytes at %d: %v", n, offset, err)
	}
	for i, b := range buf {
		if b != byte((offset+i)%251) {
			t.Fatalf("byte %d = %d, want %d", offset+i, b, (offset+i)%251)
		}
	}
}

func registered(t *testing.T, srv *Server) *guest {
	t.Helper()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	g := srv.guests["vm-1"]
	if g == nil {
		t.Fatal("no guest registered")
	}
	return g
}

func waitClosed(t *testing.T, g *guest, within time.Duration) {
	t.Helper()
	select {
	case <-g.conn.Context().Done():
	case <-time.After(within):
		t.Fatalf("the retired connection was still open after %s", within)
	}
}

// rotating a running guest's token must not break a download it is serving:
// the old connection keeps the streams it has, every new stream goes to the
// guest that dialed in with the new token, and the old connection closes once
// its last stream is done.
func TestRotatingTheTokenKeepsAnInProgressDownload(t *testing.T) {
	const size = 4 << 20
	h := startServer(t)
	h.cfg.Ports = []int{downloadServer(t, size), echoServer(t)}
	runSidecar(t, h.cfg)

	dl, err := open(t, h.srv, h.cfg.Ports[0], 5*time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dl.Close()
	_ = dl.SetDeadline(time.Now().Add(30 * time.Second))
	readPattern(t, dl, 0, 1<<20)

	old := registered(t, h.srv)
	h.token.Store("rotated")
	h.srv.Retire("vm-1")

	rotated := h.cfg
	rotated.Token = "rotated"
	runSidecar(t, rotated)
	fresh, err := open(t, h.srv, h.cfg.Ports[1], 10*time.Second)
	if err != nil {
		t.Fatalf("open after the rotation: %v", err)
	}
	defer fresh.Close()
	roundTrip(t, fresh, bufio.NewReader(fresh), "new token")
	if fresh.RemoteAddr().String() == dl.RemoteAddr().String() {
		t.Fatal("a new stream went to the retired connection")
	}
	if registered(t, h.srv) == old {
		t.Fatal("the retired guest is still registered")
	}

	readPattern(t, dl, 1<<20, size-(1<<20))
	select {
	case <-old.conn.Context().Done():
		t.Fatal("the retired connection closed before its download was closed")
	default:
	}
	_ = dl.Close()
	waitClosed(t, old, 5*time.Second)
}

// a stream that never ends must not keep a revoked connection alive.
func TestARetiredConnectionClosesAtTheEndOfItsGrace(t *testing.T) {
	h := startServer(t)
	h.srv.drainGrace = 300 * time.Millisecond
	h.cfg.Ports = []int{echoServer(t)}
	runSidecar(t, h.cfg)

	c, err := open(t, h.srv, h.cfg.Ports[0], 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := bufio.NewReader(c)
	roundTrip(t, c, r, "before")

	old := registered(t, h.srv)
	h.srv.Retire("vm-1")
	roundTrip(t, c, r, "inside the grace")
	waitClosed(t, old, 5*time.Second)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "after\n"); err == nil {
		if _, err := r.ReadString('\n'); err == nil {
			t.Fatal("the stream still worked after the grace ended")
		}
	}
}

// with nothing open there is nothing to wait for.
func TestAnIdleRetiredConnectionClosesAtOnce(t *testing.T) {
	h := startServer(t)
	runSidecar(t, h.cfg)
	deadline := time.Now().Add(5 * time.Second)
	for !h.srv.Connected("vm-1") {
		if time.Now().After(deadline) {
			t.Fatal("guest never connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	old := registered(t, h.srv)
	h.srv.Retire("vm-1")
	waitClosed(t, old, time.Second)
}

// removing an owner cuts a connection that is still draining after a token
// rotation, rather than letting it serve out its grace.
func TestDropClosesARetiredConnectionStillDraining(t *testing.T) {
	h := startServer(t)
	h.cfg.Ports = []int{echoServer(t)}
	runSidecar(t, h.cfg)

	c, err := open(t, h.srv, h.cfg.Ports[0], 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	roundTrip(t, c, bufio.NewReader(c), "before")

	old := registered(t, h.srv)
	h.srv.Retire("vm-1")
	h.srv.Drop("vm-1")
	waitClosed(t, old, time.Second)
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.retiring) != 0 {
		t.Fatalf("%d guests still tracked as retiring after the drop", len(h.srv.retiring))
	}
}

// a clone's probe of a paused guest must not close that guest if a token
// rotation retires it while the probe is still waiting: a retired guest is
// draining, not failed.
func TestAProbeThatOutlivesARetireDoesNotCloseTheRetiredGuest(t *testing.T) {
	old := keepAlive
	t.Cleanup(func() { keepAlive = old })
	keepAlive = 200 * time.Millisecond

	h := startServer(t)
	h.cfg.Ports = []int{echoServer(t)}
	rl := newRelay(t, h.srv.Addr())
	paused := h.cfg
	paused.ProxyAddr = rl.front.LocalAddr().String()
	runSidecar(t, paused)

	c, err := open(t, h.srv, h.cfg.Ports[0], 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := bufio.NewReader(c)
	roundTrip(t, c, r, "before the pause")
	retired := registered(t, h.srv)

	rl.blocked.Store(true)
	runSidecar(t, h.cfg) // the clone, which probes the paused guest
	deadline := time.Now().Add(5 * time.Second)
	for h.hellos.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the clone never said hello")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.srv.Retire("vm-1")

	// wait out the probe the clone started before the retire.
	deadline = time.Now().Add(probeTimeout + 5*time.Second)
	for {
		h.rec.mu.Lock()
		dead := h.rec.probes[false]
		h.rec.mu.Unlock()
		if dead > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the clone's probe never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-retired.conn.Context().Done():
		t.Fatalf("the probe closed the retired guest: %v", context.Cause(retired.conn.Context()))
	default:
	}

	rl.blocked.Store(false)
	roundTrip(t, c, r, "after the pause")
}
