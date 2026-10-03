package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/cli/config"
	fuse "github.com/folsomintel/fuse/sdks/go"
)

const testExposition = `# HELP orchestrator_reconcile_cycles_total Total number of reconcile cycles completed.
# TYPE orchestrator_reconcile_cycles_total counter
orchestrator_reconcile_cycles_total 7
# HELP orchestrator_operations_duration_seconds Lifecycle operation latency by op.
# TYPE orchestrator_operations_duration_seconds histogram
orchestrator_operations_duration_seconds_bucket{op="fork",le="1"} 1
orchestrator_operations_duration_seconds_bucket{op="fork",le="+Inf"} 2
orchestrator_operations_duration_seconds_sum{op="fork"} 3.5
orchestrator_operations_duration_seconds_count{op="fork"} 2
`

func TestParseMetricsFlattensHistograms(t *testing.T) {
	fams, err := parseMetrics(strings.NewReader(testExposition))
	if err != nil {
		t.Fatal(err)
	}
	if len(fams) != 2 || fams[0].Name != "orchestrator_operations_duration_seconds" {
		t.Fatalf("families = %+v", fams)
	}
	hist := fams[0]
	if hist.Type != "histogram" || len(hist.Samples) != 2 {
		t.Fatalf("histogram = %+v, want a _count and a _sum sample", hist)
	}
	if s := hist.Samples[0]; s.Name != "orchestrator_operations_duration_seconds_count" || s.Value != 2 || s.Labels["op"] != "fork" {
		t.Fatalf("count sample = %+v", s)
	}
	if s := fams[1].Samples[0]; fams[1].Type != "counter" || s.Value != 7 {
		t.Fatalf("counter = %+v", fams[1])
	}
}

// one source failing (here, a token that cannot list snapshots) is reported
// and leaves the others intact.
func TestCollectDashboardKeepsGoingPastAFailedSource(t *testing.T) {
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hosts":
			fmt.Fprint(w, `{"hosts":[{"id":"h1","state":"active","capacity":{"cpus":4},"allocated":{"cpus":1}}]}`)
		case "/v1/environments":
			fmt.Fprint(w, `{"environments":[{"id":"vm-1","state":"running","host_id":"h1"}]}`)
		case "/v1/snapshots":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":"forbidden"}`)
		case "/metrics":
			fmt.Fprint(w, testExposition)
		default:
			http.NotFound(w, r)
		}
	}))
	defer orch.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "# TYPE fuse_proxy_routes gauge\nfuse_proxy_routes 3\n")
	}))
	defer proxy.Close()

	cl, err := fuse.New(orch.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	state := collectDashboard(context.Background(), cl, &config.Context{BaseURL: orch.URL, Token: "test-token"}, proxy.URL+"/metrics")

	if len(state.Hosts) != 1 || len(state.Environments) != 1 {
		t.Fatalf("hosts %d, environments %d, want 1 each", len(state.Hosts), len(state.Environments))
	}
	if state.Errors["snapshots"] == "" || len(state.Errors) != 1 {
		t.Fatalf("errors = %v, want only snapshots", state.Errors)
	}
	if len(state.Metrics["orchestrator"]) != 2 {
		t.Fatalf("orchestrator families = %d, want 2", len(state.Metrics["orchestrator"]))
	}
	if p := state.Metrics["proxy"]; len(p) != 1 || p[0].Samples[0].Value != 3 {
		t.Fatalf("proxy families = %+v", p)
	}
}
