package egress

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// MockName is the provider name a caller uses to ask for the mock backend.
const MockName = "mock"

// Mock is a real, fully functional socks5 backend that runs inside the
// orchestrator process and egresses directly from wherever that process
// is. it provides no privacy or isolation whatsoever.
//
// it exists so the whole lifecycle (provision, health, env injection,
// teardown, and each failure mode) can be exercised without a cloudflare
// account or a configured host, and so tests have something to point at
// that is not a stub. `fuse local` runs a proxy-mode environment end to end
// with this and nothing else.
type Mock struct {
	// AllowLoopback lets tests bind 127.0.0.1. a guest cannot reach the
	// host's loopback, so production wiring leaves this false and a
	// loopback listen ip is refused.
	AllowLoopback bool
	// DialTimeout bounds each upstream dial. zero means ten seconds.
	DialTimeout time.Duration

	mu        sync.Mutex
	listeners map[string]net.Listener
	failNext  error
}

// NewMock returns a mock provider with no listeners.
func NewMock() *Mock {
	return &Mock{listeners: map[string]net.Listener{}}
}

// Name implements Provider.
func (m *Mock) Name() string { return MockName }

// FailNextProvision makes the next Provision call fail with err and
// allocate nothing: the "backend refuses to come up" case.
func (m *Mock) FailNextProvision(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNext = err
}

// Kill closes vmID's listener without forgetting it, so the next
// Healthcheck fails and Destroy still has a record to release: the "went
// unhealthy after a successful provision" case. it reports whether there
// was a listener to kill.
func (m *Mock) Kill(vmID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	ln, ok := m.listeners[vmID]
	if ok {
		_ = ln.Close()
	}
	return ok
}

// Active lists the vm ids that still hold a listener record, for tests
// asserting that teardown leaves nothing behind.
func (m *Mock) Active() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.listeners))
	for id := range m.listeners {
		ids = append(ids, id)
	}
	return ids
}

// Provision implements Provider. it binds to ec.ListenIP: never loopback
// (invisible to the guest) and never unspecified (exposes the proxy to the
// host's whole network).
func (m *Mock) Provision(_ context.Context, ec Context) (Endpoint, error) {
	if ec.Protocol != ProtocolSOCKS5 {
		return Endpoint{}, fmt.Errorf("mock speaks %s only, not %s", ProtocolSOCKS5, ec.Protocol)
	}
	ip := net.ParseIP(ec.ListenIP)
	if ip == nil {
		return Endpoint{}, fmt.Errorf("mock: listen ip %q is not an ip address", ec.ListenIP)
	}
	if ip.IsUnspecified() {
		return Endpoint{}, fmt.Errorf("mock: refusing to listen on %s, which would expose the proxy to the host's whole network", ec.ListenIP)
	}
	if ip.IsLoopback() && !m.AllowLoopback {
		return Endpoint{}, fmt.Errorf("mock: refusing to listen on %s, which the guest cannot reach", ec.ListenIP)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failNext; err != nil {
		m.failNext = nil
		return Endpoint{}, err
	}
	if _, exists := m.listeners[ec.VMID]; exists {
		return Endpoint{}, fmt.Errorf("mock: %s already has a listener; destroy it first", ec.VMID)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ec.ListenIP, "0"))
	if err != nil {
		return Endpoint{}, fmt.Errorf("mock: listen: %w", err)
	}
	m.listeners[ec.VMID] = ln
	go m.serve(ln)
	return Endpoint{
		URL:      "socks5h://" + ln.Addr().String(),
		Protocol: ProtocolSOCKS5,
		Provider: MockName,
	}, nil
}

// Healthcheck implements Provider: it dials the endpoint and completes a
// socks5 method negotiation, so a port held by something that is not a
// socks server is not reported healthy.
func (m *Mock) Healthcheck(ctx context.Context, ep Endpoint) error {
	if ep.Protocol == "" {
		ep.Protocol = ProtocolSOCKS5
	}
	return ProbeEndpoint(ctx, ep)
}

// Destroy implements Provider. it closes the listener and forgets it, and
// is a no-op the second time.
func (m *Mock) Destroy(_ context.Context, ec Context) error {
	m.mu.Lock()
	ln, ok := m.listeners[ec.VMID]
	delete(m.listeners, ec.VMID)
	m.mu.Unlock()
	if ok {
		_ = ln.Close()
	}
	return nil
}

func (m *Mock) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go m.handle(conn)
	}
}

func (m *Mock) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	target, err := socks5Connect(conn)
	if err != nil {
		return
	}
	timeout := m.DialTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	up, err := net.DialTimeout("tcp", target, timeout)
	if err != nil {
		socks5Reply(conn, socks5ReplyFor(err))
		return
	}
	defer func() { _ = up.Close() }()
	socks5Reply(conn, 0x00)
	_ = conn.SetDeadline(time.Time{})

	// pump both ways and finish when either side does; the deferred closes
	// unblock the other copy.
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(up, conn)
		if tc, ok := up.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, up)
		done <- struct{}{}
	}()
	<-done
}

// socks5Connect runs the server side of rfc 1928 up to the CONNECT request
// and returns the target as host:port. hostname targets (atyp 0x03) are
// returned unresolved, so the dial resolves them here: that is the
// socks5h path, and the one that matters once the guest has no resolver.
func socks5Connect(conn net.Conn) (string, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return "", err
	}
	if hdr[0] != 5 {
		return "", fmt.Errorf("socks version %d", hdr[0])
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return "", err
	}
	noAuth := false
	for _, mth := range methods {
		if mth == 0 {
			noAuth = true
		}
	}
	if !noAuth {
		_, _ = conn.Write([]byte{5, 0xff})
		return "", errors.New("client offers no acceptable auth method")
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return "", err
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return "", err
	}
	if req[1] != 1 {
		socks5Reply(conn, 0x07)
		return "", fmt.Errorf("socks command %d not supported", req[1])
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	case 3:
		n := make([]byte, 1)
		if _, err := io.ReadFull(conn, n); err != nil {
			return "", err
		}
		b := make([]byte, n[0])
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}
		host = string(b)
	case 4:
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	default:
		socks5Reply(conn, 0x08)
		return "", fmt.Errorf("socks address type %d not supported", req[3])
	}
	p := make([]byte, 2)
	if _, err := io.ReadFull(conn, p); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(p)))), nil
}

// socks5Reply writes a reply with the given code. the bind address is
// reported as 0.0.0.0:0; nothing that dials this proxy uses it.
func socks5Reply(conn net.Conn, code byte) {
	_, _ = conn.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
}

// socks5ReplyFor maps an upstream dial failure to the reply code a client
// can act on. the codes are rfc 1928 section 6.
func socks5ReplyFor(err error) byte {
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr):
		return 0x04 // host unreachable
	case errors.Is(err, syscall.ECONNREFUSED):
		return 0x05 // connection refused
	default:
		return 0x01 // general failure
	}
}
