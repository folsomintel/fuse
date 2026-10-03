package ingress

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// eventually polls cond, because a counter is updated just after the bytes it
// counts have already reached the other side.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// the metrics follow one owner through publish, traffic, adopt and removal,
// and the series labeled with it are gone once it is.
func TestMetricsFollowAnOwner(t *testing.T) {
	r := newRig(t, t.TempDir())
	m := r.metrics
	port := startGuest(t, "vm-1")
	grant := r.publish(t, "vm-1", port, "")
	runSidecar(t, grant, "vm-1", port)

	if got := ask(t, grant.Routes[0].URL); got != "vm-1" {
		t.Fatalf("answer = %q", got)
	}
	eventually(t, "the stream to close", func() bool {
		return testutil.ToFloat64(m.streamsOpen.WithLabelValues("vm-1")) == 0 &&
			testutil.ToFloat64(m.bytes.WithLabelValues("vm-1", "out")) == float64(len("vm-1\n"))
	})
	for _, c := range []struct {
		name      string
		got, want float64
	}{
		{"guests_connected", testutil.ToFloat64(m.guestsConnected), 1},
		{"guest_connections_total", testutil.ToFloat64(m.guestConnections.WithLabelValues("vm-1")), 1},
		{"handshakes accepted", testutil.ToFloat64(m.handshakes.WithLabelValues("accepted")), 1},
		{"connections ok", testutil.ToFloat64(m.connections.WithLabelValues("vm-1", "ok")), 1},
		{"bytes in", testutil.ToFloat64(m.bytes.WithLabelValues("vm-1", "in")), float64(len("who\n"))},
		{"routes", testutil.ToFloat64(m.routes), 1},
		{"owners", testutil.ToFloat64(m.owners), 1},
		{"publish ok", testutil.ToFloat64(m.routeOps.WithLabelValues("publish", "ok")), 1},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	// the scrape needs no admin token, like /healthz.
	res, err := http.Get(r.admin.BaseURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `fuse_proxy_bytes_total{direction="in",owner="vm-1"} 4`) {
		t.Fatalf("/metrics = %d:\n%s", res.StatusCode, body)
	}

	r.publish(t, "vm-2", port, "vm-1")
	if n := testutil.ToFloat64(m.routeOps.WithLabelValues("adopt", "ok")); n != 1 {
		t.Fatalf("adopts = %v, want 1", n)
	}
	if err := r.admin.Unpublish(context.Background(), "vm-1"); err != nil {
		t.Fatal(err)
	}
	if n := testutil.ToFloat64(m.routeOps.WithLabelValues("unpublish", "ok")); n != 1 {
		t.Fatalf("unpublishes = %v, want 1", n)
	}
	if n := testutil.ToFloat64(m.owners); n != 1 {
		t.Fatalf("owners after removing vm-1 = %v, want 1", n)
	}
	for name, n := range map[string]int{
		"connections": testutil.CollectAndCount(m.connections),
		"bytes":       testutil.CollectAndCount(m.bytes),
		"streams":     testutil.CollectAndCount(m.streamsOpen),
		"guests":      testutil.CollectAndCount(m.guestConnections),
	} {
		if n != 0 {
			t.Errorf("%s still has %d series after the owner was removed", name, n)
		}
	}
}
