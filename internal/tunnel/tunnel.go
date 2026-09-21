// Package tunnel is the link between fuse-proxy and the sidecar inside a guest.
//
// the guest dials OUT to the proxy and holds one quic connection open. every
// tcp connection a client makes to the proxy becomes one quic stream that the
// proxy opens toward the guest, and the sidecar connects that stream to a port
// on the guest's loopback. nothing ever dials in to the guest, so where the
// guest is running never appears in anything a client holds.
//
// quic is here for one property: a connection is identified by a connection
// id, not by the address it comes from. when the guest's packets start arriving
// from somewhere else (it was resumed on another host, the host's nat rebound,
// the link dropped for a while), the proxy validates the new path and carries
// on, and whatever was lost in between is retransmitted. every open stream
// survives that. the alternative was a tcp tunnel plus a hand-written protocol
// for resuming streams across reconnects, which is a reimplementation of the
// part of quic that already works.
package tunnel

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	alpn = "fuse-tunnel/1"

	// idleTimeout is how long a connection may be silent before either side
	// gives up on it, and so the longest pause a guest can take (a live
	// migration's stop-and-copy window) with its streams still intact on the
	// other side of it. it is long on purpose: the cost of a dead guest holding
	// a slot for this long is small, because a second guest claiming the same
	// identity triggers a liveness check rather than waiting this out.
	idleTimeout = 5 * time.Minute

	// keepAlive comes from the guest only. a paused guest sends nothing, and
	// that must not look like a failure to the proxy.
	keepAlive = 15 * time.Second

	// helloTimeout bounds the handshake on the control stream.
	helloTimeout = 10 * time.Second

	// probeTimeout is how long an established guest has to answer a liveness
	// probe before a newcomer with the same identity is allowed to replace it.
	probeTimeout = 3 * time.Second

	// maxStreams caps concurrent client connections per guest.
	maxStreams = 4096

	statusOK      byte = 0
	statusRefused byte = 1
)

// Config is what the sidecar needs to find the proxy and prove who it is. it is
// written into the guest as /fuse/tunnel.json by the orchestrator.
type Config struct {
	// ProxyAddr is the proxy's quic listener, host:port.
	ProxyAddr string `json:"proxy_addr"`
	// ServerCertSHA256 pins the proxy's certificate. the proxy's cert is
	// self-signed, so this fingerprint IS the trust anchor.
	ServerCertSHA256 string `json:"server_cert_sha256"`
	// Owner names the guest to the proxy; Token proves it.
	Owner string `json:"owner"`
	Token string `json:"token"`
	// Ports are the only loopback ports the sidecar will connect a stream to.
	// the proxy is trusted to route, not to reach sshd.
	Ports []int `json:"ports"`
}

// hello is the first message on the control stream, guest to proxy.
type hello struct {
	Owner string `json:"owner"`
	Token string `json:"token"`
}

// helloReply answers it. Error is empty on success.
type helloReply struct {
	Error string `json:"error,omitempty"`
}

// streamHeader opens every stream the proxy sends the guest. Port 0 is a
// liveness probe: the guest answers statusOK and the stream ends.
type streamHeader struct {
	Port int `json:"port"`
}

func quicConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:        idleTimeout,
		MaxIncomingStreams:    maxStreams,
		MaxIncomingUniStreams: -1,
	}
}

// CertFingerprint is the pin a Config carries for cert.
func CertFingerprint(cert tls.Certificate) string {
	sum := sha256.Sum256(cert.Certificate[0])
	return hex.EncodeToString(sum[:])
}

// pinnedTLS trusts exactly one certificate, by fingerprint. chain verification
// is off because there is no chain: the proxy's cert is self-signed and the
// fingerprint arrived over the orchestrator's authenticated channel.
func pinnedTLS(fingerprint string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{alpn},
		InsecureSkipVerify: true, // #nosec G402 -- replaced by the pin below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("tunnel: proxy presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			if hex.EncodeToString(sum[:]) != fingerprint {
				return errors.New("tunnel: proxy certificate does not match the pinned fingerprint")
			}
			return nil
		},
	}
}

func writeJSONLine(w io.Writer, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(raw, '\n'))
	return err
}

// readJSONLine reads one bounded line. the reader is returned to the caller's
// stream afterwards only through r, so callers keep using r.
func readJSONLine(r *bufio.Reader, v any) error {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// streamConn presents a quic stream as a net.Conn.
type streamConn struct {
	*quic.Stream
	r      *bufio.Reader
	local  net.Addr
	remote net.Addr
}

func (c *streamConn) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *streamConn) LocalAddr() net.Addr        { return c.local }
func (c *streamConn) RemoteAddr() net.Addr       { return c.remote }

// CloseWrite sends fin and leaves the read side open, like *net.TCPConn.
func (c *streamConn) CloseWrite() error { return c.Stream.Close() }

// Close ends both directions. quic's own Close only ends the write side.
func (c *streamConn) Close() error {
	c.CancelRead(0)
	return c.Stream.Close()
}

type halfCloser interface {
	CloseWrite() error
}

// Pipe copies both ways until both directions are done, passing a half close
// through so a peer that sends then waits for a reply still gets one.
func Pipe(a, b net.Conn) {
	done := make(chan struct{})
	oneWay := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if hc, ok := dst.(halfCloser); ok {
			_ = hc.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go oneWay(a, b)
	go oneWay(b, a)
	<-done
	<-done
	_ = a.Close()
	_ = b.Close()
}
