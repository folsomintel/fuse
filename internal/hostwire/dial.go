// Package hostwire holds the pieces of the orchestrator-to-host-agent wire
// that both the firecracker and qemu providers speak. Today that is the raw
// HTTP/1.1 upgrade used to open an attach stream to a guest; the JSON request
// path still lives in each provider.
package hostwire

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// handshakeTimeout bounds the upgrade exchange itself. It does not bound the
// lifetime of the stream that follows, which is as long as the guest process
// lives.
const handshakeTimeout = 15 * time.Second

// Dial opens a raw duplex connection to a host-agent endpoint by performing an
// HTTP/1.1 Upgrade by hand, and returns the socket once the host agent has
// answered 101.
//
// It deliberately bypasses http.Client. Two reasons, both structural:
//
//   - net/http gives a client no way to reclaim the underlying connection
//     after a response; only servers get Hijack. An upgrade is exactly the
//     case where the caller needs the socket back.
//   - An http.Client speaking TLS may negotiate HTTP/2, and HTTP/2 has no
//     connection upgrade at all. Writing the request onto a raw conn pins us
//     to HTTP/1.1, where the upgrade is well defined.
//
// The returned conn is positioned immediately after the 101 response, so the
// first byte read from it is the first byte of the stream proper.
func Dial(ctx context.Context, baseURL, token, path, proto string) (net.Conn, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/") + path)
	if err != nil {
		return nil, fmt.Errorf("attach: bad host url: %w", err)
	}

	host := u.Host
	if u.Port() == "" {
		switch u.Scheme {
		case "https":
			host = net.JoinHostPort(host, "443")
		default:
			host = net.JoinHostPort(host, "80")
		}
	}

	var c net.Conn
	if u.Scheme == "https" {
		d := &tls.Dialer{Config: &tls.Config{ServerName: u.Hostname()}}
		c, err = d.DialContext(ctx, "tcp", host)
	} else {
		d := &net.Dialer{}
		c, err = d.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, fmt.Errorf("attach: dial host: %w", err)
	}

	// Bound the handshake, then hand a deadline-free socket to the caller: the
	// stream that follows is interactive and may idle for as long as a human
	// stares at a shell prompt.
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	} else {
		_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	}

	req := &http.Request{
		Method: http.MethodGet,
		URL:    u,
		Host:   u.Host,
		Header: http.Header{
			"Connection": {"Upgrade"},
			"Upgrade":    {proto},
		},
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if err := req.Write(c); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("attach: write upgrade request: %w", err)
	}

	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("attach: read upgrade response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = c.Close()
		return nil, fmt.Errorf("attach: host agent refused upgrade: http %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	_ = c.SetDeadline(time.Time{})

	// http.ReadResponse may have pulled stream bytes into br's buffer along
	// with the response head. Reads must therefore go through br, not the raw
	// conn, or those bytes are silently dropped.
	return &bufferedConn{Conn: c, r: br}, nil
}

// bufferedConn reads through the bufio.Reader that consumed the 101 response
// while writing straight to the socket.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
