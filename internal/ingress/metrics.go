package ingress

import (
	"net"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/folsomintel/fuse/internal/tunnel"
)

// Metrics is fuse-proxy's prometheus metrics. a nil *Metrics records nothing,
// so a Proxy built without one (the tests) needs no special casing.
//
// series labeled by owner are deleted when the owner is removed, so the label
// set is bounded by the live fleet rather than by every vm ever published.
type Metrics struct {
	guestsConnected   prometheus.Gauge
	handshakes        *prometheus.CounterVec
	guestConnections  *prometheus.CounterVec
	guestReplacements prometheus.Counter
	probes            *prometheus.CounterVec
	pathMigrations    *prometheus.CounterVec

	owners         prometheus.Gauge
	routes         prometheus.Gauge
	routeOps       *prometheus.CounterVec
	portExhaustion prometheus.Counter
	connections    *prometheus.CounterVec
	streamsOpen    *prometheus.GaugeVec
	bytes          *prometheus.CounterVec
}

var _ tunnel.Metrics = (*Metrics)(nil)

// NewMetrics creates and registers fuse-proxy's metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		guestsConnected: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "fuse_proxy", Subsystem: "tunnel", Name: "guests_connected",
			Help: "Guests currently holding a tunnel connection.",
		}),
		handshakes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "fuse_proxy", Subsystem: "tunnel", Name: "handshakes_total",
			Help: "Guest hellos by result: accepted, unauthorized, duplicate (a live guest holds the owner), bad_hello.",
		}, []string{"result"}),
		guestConnections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "fuse_proxy", Subsystem: "tunnel", Name: "guest_connections_total",
			Help: "Fresh tunnel connections accepted per owner. a live migrate that kept its connection does not move this.",
		}, []string{"owner"}),
		guestReplacements: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "fuse_proxy", Subsystem: "tunnel", Name: "guest_replacements_total",
			Help: "Established guests evicted because they failed a liveness probe when a second guest claimed their identity.",
		}),
		probes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "fuse_proxy", Subsystem: "tunnel", Name: "probes_total",
			Help: "Clone-identity liveness probes of an established guest, by result: alive, dead.",
		}, []string{"result"}),
		pathMigrations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "fuse_proxy", Subsystem: "tunnel", Name: "path_migrations_total",
			Help: "Times a guest's connection moved to a new address without a new handshake, per owner.",
		}, []string{"owner"}),

		owners: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "fuse_proxy", Name: "owners",
			Help: "Owners (vms) with published routes.",
		}),
		routes: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "fuse_proxy", Name: "routes",
			Help: "Public ports currently published.",
		}),
		routeOps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "fuse_proxy", Name: "route_operations_total",
			Help: "Admin operations by op (publish, adopt, unpublish) and result (ok, error).",
		}, []string{"op", "result"}),
		portExhaustion: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "fuse_proxy", Name: "port_exhaustion_total",
			Help: "Publishes that failed because the public port range had no free port.",
		}),
		connections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "fuse_proxy", Name: "connections_total",
			Help: "Client connections per owner by result: ok, failed (the guest was not reachable within the hold timeout, or refused the port).",
		}, []string{"owner", "result"}),
		streamsOpen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "fuse_proxy", Name: "streams_open",
			Help: "Client connections currently piped to a guest, per owner.",
		}, []string{"owner"}),
		bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "fuse_proxy", Name: "bytes_total",
			Help: "Bytes carried per owner by direction: in (client to guest), out (guest to client).",
		}, []string{"owner", "direction"}),
	}
	reg.MustRegister(
		m.guestsConnected, m.handshakes, m.guestConnections, m.guestReplacements, m.probes, m.pathMigrations,
		m.owners, m.routes, m.routeOps, m.portExhaustion, m.connections, m.streamsOpen, m.bytes,
	)
	return m
}

// Handshake implements tunnel.Metrics.
func (m *Metrics) Handshake(result string) {
	if m == nil {
		return
	}
	m.handshakes.WithLabelValues(result).Inc()
}

// GuestConnected implements tunnel.Metrics.
func (m *Metrics) GuestConnected(owner string, replaced bool) {
	if m == nil {
		return
	}
	m.guestConnections.WithLabelValues(owner).Inc()
	if replaced {
		m.guestReplacements.Inc()
	}
}

// GuestsConnected implements tunnel.Metrics.
func (m *Metrics) GuestsConnected(n int) {
	if m == nil {
		return
	}
	m.guestsConnected.Set(float64(n))
}

// Probe implements tunnel.Metrics.
func (m *Metrics) Probe(alive bool) {
	if m == nil {
		return
	}
	result := "dead"
	if alive {
		result = "alive"
	}
	m.probes.WithLabelValues(result).Inc()
}

// PathMigrated implements tunnel.Metrics.
func (m *Metrics) PathMigrated(owner string) {
	if m == nil {
		return
	}
	m.pathMigrations.WithLabelValues(owner).Inc()
}

func (m *Metrics) routeOp(op string, err error) {
	if m == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.routeOps.WithLabelValues(op, result).Inc()
}

func (m *Metrics) exhausted() {
	if m == nil {
		return
	}
	m.portExhaustion.Inc()
}

func (m *Metrics) setRoutes(owners, routes int) {
	if m == nil {
		return
	}
	m.owners.Set(float64(owners))
	m.routes.Set(float64(routes))
}

func (m *Metrics) connectFailed(owner string) {
	if m == nil {
		return
	}
	m.connections.WithLabelValues(owner, "failed").Inc()
}

// stream counts one client connection to owner's guest and returns the
// connection wrapped to count its bytes, plus a func to call when it ends.
// the children are looked up once here: if the owner is removed while the
// stream runs, its updates land on detached series instead of recreating
// the ones forgetOwner just deleted.
func (m *Metrics) stream(owner string, client net.Conn) (net.Conn, func()) {
	if m == nil {
		return client, func() {}
	}
	m.connections.WithLabelValues(owner, "ok").Inc()
	open := m.streamsOpen.WithLabelValues(owner)
	open.Inc()
	counted := &countingConn{
		Conn: client,
		in:   m.bytes.WithLabelValues(owner, "in"),
		out:  m.bytes.WithLabelValues(owner, "out"),
	}
	return counted, open.Dec
}

// forgetOwner drops every series labeled with owner.
func (m *Metrics) forgetOwner(owner string) {
	if m == nil {
		return
	}
	labels := prometheus.Labels{"owner": owner}
	m.guestConnections.DeletePartialMatch(labels)
	m.pathMigrations.DeletePartialMatch(labels)
	m.connections.DeletePartialMatch(labels)
	m.streamsOpen.DeletePartialMatch(labels)
	m.bytes.DeletePartialMatch(labels)
}

// countingConn counts the bytes read from and written to a client as they
// move, so a long-lived connection shows up before it ends.
type countingConn struct {
	net.Conn
	in, out prometheus.Counter
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.in.Add(float64(n))
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.out.Add(float64(n))
	return n, err
}

// CloseWrite keeps tunnel.Pipe's half close working through the wrapper.
func (c *countingConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return c.Close()
}
