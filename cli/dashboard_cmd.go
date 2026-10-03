package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/spf13/cobra"

	"github.com/folsomintel/fuse/cli/config"
	fuse "github.com/folsomintel/fuse/sdks/go"
)

//go:embed dashboard/index.html
var dashboardPage []byte

func newDashboardCmd() *cobra.Command {
	var (
		listen       string
		proxyMetrics string
		noOpen       bool
	)
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Serve a local page with hosts, environments, snapshots and metrics",
		Long: "dashboard serves a page on localhost that shows the whole fleet behind the\n" +
			"active context: every host with its capacity, every environment, every\n" +
			"snapshot, and the orchestrator's Prometheus metrics, refreshed every few\n" +
			"seconds. it is a dev tool: nothing is stored, and it reads only what the\n" +
			"context's token can already read.\n\n" +
			"    fuse dashboard\n" +
			"    fuse dashboard --proxy-metrics http://127.0.0.1:7080/metrics\n\n" +
			"--proxy-metrics adds fuse-proxy's metrics. its admin listener is loopback on\n" +
			"the proxy host by default, so forward it first (ssh -L 7080:127.0.0.1:7080).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDashboard(cmd.Context(), listen, proxyMetrics, noOpen)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:0", "local address to serve the dashboard on")
	cmd.Flags().StringVar(&proxyMetrics, "proxy-metrics", "", "fuse-proxy /metrics url to include (optional)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the dashboard url without opening a browser")
	return cmd
}

func runDashboard(ctx context.Context, listen, proxyMetrics string, noOpen bool) error {
	cl, cur, err := app.client()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// the page binds localhost, but any local process could still reach the
	// port; a per-invocation token in the url keeps the fleet's state to
	// whoever ran the command, as `fuse desktop` does.
	token, err := randomHex(16)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listen, err)
	}

	ctxName := app.ctxName
	if ctxName == "" {
		ctxName = app.cfg.CurrentContext
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(dashboardPage)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(token)) != 1 {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		state := collectDashboard(r.Context(), cl, cur, proxyMetrics)
		state.Context = ctxName
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	pageURL := fmt.Sprintf("http://%s/?token=%s", ln.Addr(), token)
	fmt.Printf("fleet dashboard for %s: %s\n", cur.BaseURL, pageURL)
	if !noOpen {
		if err := openBrowser(pageURL); err != nil {
			fmt.Printf("could not open a browser (%v); open the url yourself\n", err)
		}
	}
	fmt.Println("serving until interrupted; press ctrl-c to stop")

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return nil
}

// dashboardState is one refresh of the page. a source that fails leaves its
// field empty and says why in Errors, so one unreachable piece (a token that
// cannot list hosts, a proxy that is not forwarded) does not blank the rest.
type dashboardState struct {
	Context      string                    `json:"context"`
	BaseURL      string                    `json:"base_url"`
	FetchedAt    time.Time                 `json:"fetched_at"`
	Hosts        []fuse.Host               `json:"hosts"`
	Environments []fuse.EnvironmentInfo    `json:"environments"`
	Snapshots    []fuse.Snapshot           `json:"snapshots"`
	Metrics      map[string][]metricFamily `json:"metrics"`
	Errors       map[string]string         `json:"errors,omitempty"`
}

func collectDashboard(ctx context.Context, cl *fuse.Client, cur *config.Context, proxyMetrics string) dashboardState {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	state := dashboardState{
		BaseURL:   cur.BaseURL,
		FetchedAt: time.Now().UTC(),
		Metrics:   map[string][]metricFamily{},
		Errors:    map[string]string{},
	}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	fetch := func(name string, f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil {
				mu.Lock()
				state.Errors[name] = friendly(err).Error()
				mu.Unlock()
			}
		}()
	}
	fetch("hosts", func() error {
		hosts, err := cl.Hosts.List(ctx)
		mu.Lock()
		state.Hosts = hosts
		mu.Unlock()
		return err
	})
	fetch("environments", func() error {
		envs, err := cl.Environments.List(ctx, fuse.ListEnvironmentsOptions{})
		mu.Lock()
		state.Environments = envs
		mu.Unlock()
		return err
	})
	fetch("snapshots", func() error {
		snaps, err := cl.Snapshots.List(ctx, fuse.ListSnapshotsOptions{})
		mu.Lock()
		state.Snapshots = snaps
		mu.Unlock()
		return err
	})
	fetch("orchestrator metrics", func() error {
		body, err := scrapeOrchestratorMetrics(ctx, cur)
		if err != nil {
			return err
		}
		fams, err := parseMetrics(bytes.NewReader(body))
		mu.Lock()
		state.Metrics["orchestrator"] = fams
		mu.Unlock()
		return err
	})
	if proxyMetrics != "" {
		fetch("proxy metrics", func() error {
			fams, err := scrapeMetrics(ctx, proxyMetrics)
			mu.Lock()
			state.Metrics["proxy"] = fams
			mu.Unlock()
			return err
		})
	}
	wg.Wait()
	return state
}

// scrapeMetrics fetches and parses a /metrics url that needs no token.
func scrapeMetrics(ctx context.Context, rawURL string) ([]metricFamily, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid metrics url %q", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d", rawURL, resp.StatusCode)
	}
	return parseMetrics(resp.Body)
}

// metricFamily is a parsed family flattened for the page: a histogram or
// summary becomes its _count and _sum samples, which is enough to show a rate
// and a mean without shipping every bucket.
type metricFamily struct {
	Name    string         `json:"name"`
	Help    string         `json:"help,omitempty"`
	Type    string         `json:"type"`
	Samples []metricSample `json:"samples"`
}

type metricSample struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
}

func parseMetrics(r io.Reader) ([]metricFamily, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	parsed, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return nil, fmt.Errorf("parse metrics: %w", err)
	}
	out := make([]metricFamily, 0, len(parsed))
	for name, fam := range parsed {
		f := metricFamily{
			Name: name,
			Help: fam.GetHelp(),
			Type: lowerType(fam.GetType()),
		}
		for _, m := range fam.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			add := func(sampleName string, v float64) {
				f.Samples = append(f.Samples, metricSample{Name: sampleName, Labels: labels, Value: v})
			}
			switch fam.GetType() {
			case dto.MetricType_COUNTER:
				add(name, m.GetCounter().GetValue())
			case dto.MetricType_GAUGE:
				add(name, m.GetGauge().GetValue())
			case dto.MetricType_HISTOGRAM, dto.MetricType_GAUGE_HISTOGRAM:
				add(name+"_count", float64(m.GetHistogram().GetSampleCount()))
				add(name+"_sum", m.GetHistogram().GetSampleSum())
			case dto.MetricType_SUMMARY:
				add(name+"_count", float64(m.GetSummary().GetSampleCount()))
				add(name+"_sum", m.GetSummary().GetSampleSum())
			default:
				add(name, m.GetUntyped().GetValue())
			}
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func lowerType(t dto.MetricType) string {
	switch t {
	case dto.MetricType_COUNTER:
		return "counter"
	case dto.MetricType_GAUGE:
		return "gauge"
	case dto.MetricType_HISTOGRAM, dto.MetricType_GAUGE_HISTOGRAM:
		return "histogram"
	case dto.MetricType_SUMMARY:
		return "summary"
	default:
		return "untyped"
	}
}
