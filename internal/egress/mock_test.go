package egress

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestMock(t *testing.T) *Mock {
	t.Helper()
	m := NewMock()
	m.AllowLoopback = true
	return m
}

func loopbackContext(vmID string) Context {
	return Context{VMID: vmID, HostID: "h-1", ListenIP: "127.0.0.1", GuestIP: "127.0.0.1", Protocol: ProtocolSOCKS5}
}

// socks5Dial is a minimal client for the tests. it connects through the
// proxy at proxyURL to target, sending the host as a name (atyp 0x03) so
// the proxy is the one resolving it: the socks5h path. it returns the
// connection and the proxy's reply code.
func socks5Dial(t *testing.T, proxyURL, target string) (net.Conn, byte) {
	t.Helper()
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy %s: %v", u.Host, err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if greeting[0] != 5 || greeting[1] != 0 {
		t.Fatalf("greeting reply = %v, want [5 0]", greeting)
	}
	req := []byte{5, 1, 0, 3, byte(len(host))}
	req = append(req, host...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read connect reply: %v", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, reply[1]
}

func TestMockRefusesUnreachableOrUnsafeListenIPs(t *testing.T) {
	m := NewMock()
	cases := []struct {
		name    string
		ec      Context
		wantErr string
	}{
		{"unspecified", Context{VMID: "vm", ListenIP: "0.0.0.0", Protocol: ProtocolSOCKS5}, "whole network"},
		{"loopback without opt-in", Context{VMID: "vm", ListenIP: "127.0.0.1", Protocol: ProtocolSOCKS5}, "cannot reach"},
		{"not an ip", Context{VMID: "vm", ListenIP: "tap0", Protocol: ProtocolSOCKS5}, "not an ip address"},
		{"http protocol", Context{VMID: "vm", ListenIP: "10.200.3.1", Protocol: ProtocolHTTP}, "socks5 only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.Provision(context.Background(), tc.ec)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
	if got := m.Active(); len(got) != 0 {
		t.Fatalf("refused provisions left listeners behind: %v", got)
	}
}

func TestMockFetchesThroughTheListenerByHostname(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello via "+r.Host)
	}))
	defer upstream.Close()

	m := newTestMock(t)
	ec := loopbackContext("vm-1")
	ep, err := m.Provision(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy(context.Background(), ec) })

	if !strings.HasPrefix(ep.URL, "socks5h://127.0.0.1:") || ep.Protocol != ProtocolSOCKS5 || ep.Provider != MockName {
		t.Fatalf("endpoint = %+v, want a socks5h loopback url stamped mock/socks5", ep)
	}

	// route every dial through the proxy, naming the upstream as localhost
	// so the name reaches the proxy unresolved.
	_, port, _ := net.SplitHostPort(upstream.Listener.Addr().String())
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			conn, code := socks5Dial(t, ep.URL, addr)
			if code != 0 {
				return nil, errors.New("socks reply " + strconv.Itoa(int(code)))
			}
			return conn, nil
		},
	}}
	resp, err := client.Get("http://localhost:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello via localhost:"+port {
		t.Fatalf("body = %q, want the upstream's greeting", body)
	}
}

func TestMockReportsARefusedUpstream(t *testing.T) {
	m := newTestMock(t)
	ec := loopbackContext("vm-1")
	ep, err := m.Provision(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy(context.Background(), ec) })

	// a port that was just listening and is now closed is refused, not
	// filtered, so the reply is deterministic.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := closed.Addr().String()
	_ = closed.Close()

	conn, code := socks5Dial(t, ep.URL, target)
	_ = conn.Close()
	if code != 0x05 {
		t.Fatalf("reply code = %#x, want 0x05 connection refused", code)
	}
}

func TestMockHealthcheckFollowsTheListener(t *testing.T) {
	m := newTestMock(t)
	ec := loopbackContext("vm-1")
	ep, err := m.Provision(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy(context.Background(), ec) })

	if err := m.Healthcheck(context.Background(), ep); err != nil {
		t.Fatalf("healthy listener reported %v", err)
	}
	if !m.Kill("vm-1") {
		t.Fatal("Kill found nothing to kill")
	}
	err = m.Healthcheck(context.Background(), ep)
	if err == nil || !strings.Contains(err.Error(), "dial") {
		t.Fatalf("killed listener reported %v, want a dial failure with a reason", err)
	}
	// the record survives the kill so teardown still releases it.
	if got := m.Active(); len(got) != 1 || got[0] != "vm-1" {
		t.Fatalf("Active() = %v after kill, want [vm-1]", got)
	}
	if m.Kill("vm-1") != true {
		t.Fatal("second Kill on a recorded listener should still report true")
	}
	if m.Kill("vm-2") {
		t.Fatal("Kill of an unknown vm reported true")
	}
}

func TestMockHealthcheckRejectsANonSocksListener(t *testing.T) {
	// something else holding the port must not read as a healthy proxy.
	other := httptest.NewServer(http.NotFoundHandler())
	defer other.Close()
	m := newTestMock(t)
	// an http server never answers a socks greeting, so the probe only ends
	// at its deadline; keep that short here.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := m.Healthcheck(ctx, Endpoint{URL: "socks5h://" + other.Listener.Addr().String()})
	if err == nil {
		t.Fatal("http server reported as a healthy socks5 proxy")
	}
}

func TestMockDestroyReleasesAndIsIdempotent(t *testing.T) {
	m := newTestMock(t)
	ec := loopbackContext("vm-1")
	ep, err := m.Provision(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Destroy(context.Background(), ec); err != nil {
		t.Fatal(err)
	}
	if got := m.Active(); len(got) != 0 {
		t.Fatalf("Active() = %v after destroy, want none", got)
	}
	u, _ := url.Parse(ep.URL)
	if conn, err := net.DialTimeout("tcp", u.Host, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("listener still accepting after destroy")
	}
	if err := m.Destroy(context.Background(), ec); err != nil {
		t.Fatalf("second destroy = %v, want nil", err)
	}
	// the vm can be provisioned again once released.
	if _, err := m.Provision(context.Background(), ec); err != nil {
		t.Fatalf("re-provision after destroy: %v", err)
	}
	_ = m.Destroy(context.Background(), ec)
}

func TestMockFailNextProvisionAllocatesNothing(t *testing.T) {
	m := newTestMock(t)
	ec := loopbackContext("vm-1")
	boom := errors.New("backend refused to start")
	m.FailNextProvision(boom)
	if _, err := m.Provision(context.Background(), ec); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if got := m.Active(); len(got) != 0 {
		t.Fatalf("failed provision left a listener: %v", got)
	}
	// the hook is one-shot.
	if _, err := m.Provision(context.Background(), ec); err != nil {
		t.Fatalf("provision after the one-shot failure: %v", err)
	}
	_ = m.Destroy(context.Background(), ec)
}

func TestMockRejectsDoubleProvision(t *testing.T) {
	m := newTestMock(t)
	ec := loopbackContext("vm-1")
	if _, err := m.Provision(context.Background(), ec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Destroy(context.Background(), ec) })
	_, err := m.Provision(context.Background(), ec)
	if err == nil || !strings.Contains(err.Error(), "already has a listener") {
		t.Fatalf("err = %v, want a double-provision refusal", err)
	}
}

func TestMockThroughRegistry(t *testing.T) {
	m := newTestMock(t)
	r := NewRegistry(m)
	ec := Context{VMID: "vm-1", ListenIP: "127.0.0.1", GuestIP: "127.0.0.1"}
	ep, err := r.Provision(context.Background(), Spec{Mode: ModeProxy, Provider: MockName}, ec)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Provider != MockName || ep.Protocol != ProtocolSOCKS5 {
		t.Fatalf("endpoint = %+v", ep)
	}
	if err := r.Healthcheck(context.Background(), ep); err != nil {
		t.Fatal(err)
	}
	if err := r.Destroy(context.Background(), ep.Provider, ec); err != nil {
		t.Fatal(err)
	}
	if got := m.Active(); len(got) != 0 {
		t.Fatalf("Active() = %v after registry destroy, want none", got)
	}
}
