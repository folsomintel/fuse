package tunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// ErrNoTunnel means the owner's guest is not connected right now.
var ErrNoTunnel = errors.New("tunnel: guest is not connected")

// Authenticator reports whether token is the current credential for owner.
type Authenticator func(owner, token string) bool

// Metrics receives what the server observes. it is an interface so this
// package, which the guest sidecar also links, carries no metrics library.
type Metrics interface {
	// Handshake reports how a guest's hello ended: "accepted",
	// "unauthorized", "duplicate" (a live guest already holds the owner) or
	// "bad_hello". no owner is passed, because before it is authenticated the
	// owner is whatever the dialer chose to write.
	Handshake(result string)
	// GuestConnected reports an accepted guest. replaced is true when it took
	// the place of an established guest that failed its liveness probe.
	GuestConnected(owner string, replaced bool)
	// GuestsConnected is the number of registered guests after any change.
	GuestsConnected(n int)
	// Probe reports a liveness probe of an established guest, run when a
	// second guest claims its identity (the clone-identity check).
	Probe(alive bool)
	// PathMigrated reports that a registered guest's packets now arrive from
	// a new address on the same connection: the guest moved and every stream
	// it carried survived, with no new handshake.
	PathMigrated(owner string)
}

type nopMetrics struct{}

func (nopMetrics) Handshake(string)            {}
func (nopMetrics) GuestConnected(string, bool) {}
func (nopMetrics) GuestsConnected(int)         {}
func (nopMetrics) Probe(bool)                  {}
func (nopMetrics) PathMigrated(string)         {}

// Server is the proxy's end: it accepts guests and opens streams toward them.
type Server struct {
	ln      *quic.Listener
	auth    Authenticator
	logger  *slog.Logger
	metrics Metrics
	// pathCheck is how often a registered guest's address is compared with
	// the last one seen. a field so a test can shorten it.
	pathCheck time.Duration

	mu      sync.Mutex
	guests  map[string]*guest
	changed chan struct{} // closed and replaced whenever guests changes
}

// guest is one registered connection and the client streams open on it.
type guest struct {
	conn *quic.Conn

	mu      sync.Mutex
	streams int
}

// acquire counts a stream about to be opened on g. the server calls it under
// its own lock while g is still registered.
func (g *guest) acquire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.streams++
}

// release is the matching end of acquire, once that stream is closed or
// failed to open.
func (g *guest) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.streams--
}

// trackedConn releases its guest's stream count once, on the first Close.
type trackedConn struct {
	*streamConn
	once    sync.Once
	release func()
}

func (c *trackedConn) Close() error {
	err := c.streamConn.Close()
	c.once.Do(c.release)
	return err
}

// Listen starts accepting guests on conn.
func Listen(conn net.PacketConn, cert tls.Certificate, auth Authenticator, logger *slog.Logger) (*Server, error) {
	tr := &quic.Transport{Conn: conn}
	ln, err := tr.Listen(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{alpn},
	}, quicConfig())
	if err != nil {
		return nil, err
	}
	return &Server{
		ln:        ln,
		auth:      auth,
		logger:    logger,
		metrics:   nopMetrics{},
		pathCheck: time.Second,
		guests:    make(map[string]*guest),
		changed:   make(chan struct{}),
	}, nil
}

// SetMetrics installs m. call it before Serve.
func (s *Server) SetMetrics(m Metrics) { s.metrics = m }

// Addr is the udp address guests dial.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Serve accepts guests until ctx is done.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.ln.Close()
	}()
	for {
		conn, err := s.ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.admit(ctx, conn)
	}
}

// admit runs the handshake and, on success, keeps the guest registered until
// its connection ends.
func (s *Server) admit(ctx context.Context, conn *quic.Conn) {
	hctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	control, err := conn.AcceptStream(hctx)
	if err != nil {
		s.metrics.Handshake("bad_hello")
		_ = conn.CloseWithError(1, "no hello")
		return
	}
	_ = control.SetDeadline(time.Now().Add(helloTimeout))
	var h hello
	if err := readJSONLine(bufio.NewReaderSize(control, 4096), &h); err != nil {
		s.metrics.Handshake("bad_hello")
		_ = conn.CloseWithError(1, "bad hello")
		return
	}
	reject := func(reason string) {
		// the reply is written before the close so the guest can log why,
		// which is the difference between "fix the token" and "wait your turn".
		_ = writeJSONLine(control, helloReply{Error: reason})
		_ = control.Close()
		time.Sleep(100 * time.Millisecond)
		_ = conn.CloseWithError(1, reason)
	}
	if h.Owner == "" || !s.auth(h.Owner, h.Token) {
		s.metrics.Handshake("unauthorized")
		reject("unauthorized")
		return
	}
	registered, replaced := s.register(ctx, h.Owner, conn)
	if !registered {
		s.metrics.Handshake("duplicate")
		reject("owner already connected")
		return
	}
	if err := writeJSONLine(control, helloReply{}); err != nil {
		s.unregister(h.Owner, conn)
		return
	}
	_ = control.SetDeadline(time.Time{})
	s.metrics.Handshake("accepted")
	s.metrics.GuestConnected(h.Owner, replaced)
	s.logger.Info("guest connected", "owner", h.Owner, "from", conn.RemoteAddr().String())

	s.watchPath(h.Owner, conn)
	s.unregister(h.Owner, conn)
	s.logger.Info("guest disconnected", "owner", h.Owner, "cause", context.Cause(conn.Context()))
}

// register installs conn as owner's guest. an owner has one guest at a time.
//
// when one is already registered the newcomer does not simply win. a disk
// snapshot carries tunnel.json with it, so a fork or a migrated copy boots
// holding its source's identity and dials in with it while the source is still
// serving; newest-wins would hand that copy the source's traffic, and the two
// would then take turns evicting each other. the established guest is probed
// instead, and only one that fails to answer is replaced. that is also what
// lets a guest that crashed come back without waiting out idleTimeout.
//
// replaced reports whether an established guest was evicted to make room.
func (s *Server) register(ctx context.Context, owner string, conn *quic.Conn) (registered, replaced bool) {
	s.mu.Lock()
	var existing *quic.Conn
	if g := s.guests[owner]; g != nil {
		existing = g.conn
	}
	s.mu.Unlock()

	if existing != nil {
		alive := s.probe(ctx, existing)
		s.metrics.Probe(alive)
		if alive {
			return false, false
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.guests[owner]; current != nil && current.conn != existing {
		// somebody else registered while we were probing.
		return false, false
	}
	if existing != nil {
		_ = existing.CloseWithError(2, "replaced")
	}
	s.guests[owner] = &guest{conn: conn}
	s.broadcastLocked()
	return true, existing != nil
}

// watchPath blocks until conn ends, reporting each time the guest's packets
// start arriving from a new address. quic follows the move on its own; this
// is only so an operator can see that a migrated guest kept its connection
// rather than reconnecting.
func (s *Server) watchPath(owner string, conn *quic.Conn) {
	last := conn.RemoteAddr().String()
	tick := time.NewTicker(s.pathCheck)
	defer tick.Stop()
	for {
		select {
		case <-conn.Context().Done():
			return
		case <-tick.C:
			if now := conn.RemoteAddr().String(); now != last {
				s.metrics.PathMigrated(owner)
				s.logger.Info("guest moved", "owner", owner, "from", last, "to", now)
				last = now
			}
		}
	}
}

func (s *Server) unregister(owner string, conn *quic.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.guests[owner]; g != nil && g.conn == conn {
		delete(s.guests, owner)
		s.broadcastLocked()
	}
}

func (s *Server) broadcastLocked() {
	s.metrics.GuestsConnected(len(s.guests))
	close(s.changed)
	s.changed = make(chan struct{})
}

// probe reports whether the guest behind conn answers.
func (s *Server) probe(ctx context.Context, conn *quic.Conn) bool {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	c, err := openStream(pctx, conn, 0)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// Connected reports whether owner's guest is registered.
func (s *Server) Connected(owner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.guests[owner] != nil
}

// Drop disconnects owner's guest, if any. used when the owner is deleted or
// its token is replaced, so a revoked credential stops carrying traffic now
// rather than whenever the connection next ends.
func (s *Server) Drop(owner string) {
	s.mu.Lock()
	g := s.guests[owner]
	delete(s.guests, owner)
	if g != nil {
		s.broadcastLocked()
	}
	s.mu.Unlock()
	if g != nil {
		_ = g.conn.CloseWithError(3, "owner removed")
	}
}

// Open connects to port on owner's guest, waiting until ctx is done for the
// guest to be there and to answer.
//
// the waiting is the point. between a migrate repointing a route and the new
// guest's sidecar dialing in there is a moment with no guest at all, and a
// paused guest is registered but silent. a client that connects in either
// window is held rather than refused, and finds a working connection once the
// guest is back.
func (s *Server) Open(ctx context.Context, owner string, port int) (net.Conn, error) {
	for {
		s.mu.Lock()
		g := s.guests[owner]
		if g != nil {
			g.acquire()
		}
		changed := s.changed
		s.mu.Unlock()

		if g != nil {
			c, err := openStream(ctx, g.conn, port)
			if err == nil {
				return &trackedConn{streamConn: c, release: g.release}, nil
			}
			g.release()
			if errors.Is(err, errRefused) {
				return nil, err
			}
			if ctx.Err() != nil {
				return nil, ErrNoTunnel
			}
			// that guest went away underneath us; wait for the next one.
		}
		select {
		case <-ctx.Done():
			return nil, ErrNoTunnel
		case <-changed:
		case <-time.After(time.Second):
		}
	}
}

var errRefused = errors.New("tunnel: guest refused the port")

// openStream opens one stream toward the guest and waits for it to accept.
func openStream(ctx context.Context, conn *quic.Conn, port int) (*streamConn, error) {
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	abort := func(err error) (*streamConn, error) {
		stream.CancelRead(0)
		stream.CancelWrite(0)
		return nil, err
	}
	if err := writeJSONLine(stream, streamHeader{Port: port}); err != nil {
		return abort(err)
	}
	r := bufio.NewReaderSize(stream, 32<<10)
	status, err := r.ReadByte()
	if err != nil {
		return abort(err)
	}
	if status != statusOK {
		return abort(errRefused)
	}
	_ = stream.SetDeadline(time.Time{})
	return &streamConn{Stream: stream, r: r, local: conn.LocalAddr(), remote: conn.RemoteAddr()}, nil
}
