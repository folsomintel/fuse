package egress

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"
)

// ProbeEndpoint checks that ep is answering: a tcp dial, and for a socks5
// endpoint a full method negotiation, so a port held by something that is
// not a socks server is not reported healthy. an http CONNECT endpoint gets
// the dial only, since a CONNECT needs a target and a probe must never
// reach out through the proxy. the error is the reason that surfaces on the
// degraded environment and names only the endpoint host, which is a
// host-local address, never a destination.
func ProbeEndpoint(ctx context.Context, ep Endpoint) error {
	u, err := url.Parse(ep.URL)
	if err != nil {
		return fmt.Errorf("endpoint url: %w", err)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return fmt.Errorf("dial %s: %w", u.Host, err)
	}
	defer func() { _ = conn.Close() }()
	if ep.Protocol == ProtocolHTTP {
		return nil
	}
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return fmt.Errorf("socks5 greeting to %s: %w", u.Host, err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks5 greeting to %s: %w", u.Host, err)
	}
	if reply[0] != 5 || reply[1] != 0 {
		return fmt.Errorf("socks5 greeting to %s: unexpected reply %v", u.Host, reply)
	}
	return nil
}
