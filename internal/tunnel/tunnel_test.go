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

type harness struct {
	srv    *Server
	hellos atomic.Int32
	cfg    Config
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
	h := &harness{}
	h.srv, err = Listen(udp, cert, func(owner, token string) bool {
		h.hellos.Add(1)
		return owner == "vm-1" && token == "secret"
	}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = udp.Close() })
	go func() { _ = h.srv.Serve(ctx) }()
	h.cfg = Config{
		ProxyAddr:        h.srv.Addr().String(),
		ServerCertSHA256: CertFingerprint(cert),
		Owner:            "vm-1",
		Token:            "secret",
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
		"pin":   func(c *Config) { c.ServerCertSHA256 = "00" },
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
			_, _ = back.WriteTo(buf[:n], server)
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
			if client != nil {
				_, _ = r.front.WriteTo(buf[:n], client)
			}
		}
	}()
}

// the property the whole design rests on: the guest's address changes under an
// open stream and the stream carries on, on the same connection.
func TestAnOpenStreamSurvivesTheGuestChangingAddress(t *testing.T) {
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
}
