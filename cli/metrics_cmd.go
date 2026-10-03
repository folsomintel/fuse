package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/folsomintel/fuse/cli/config"
	fuse "github.com/folsomintel/fuse/sdks/go"
)

func newMetricsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "metrics",
		Short: "Print fleet-wide Prometheus metrics from the orchestrator",
		Long: "metrics scrapes the orchestrator's /metrics endpoint and prints the raw\n" +
			"Prometheus exposition (fleet-level series, including per-host capacity).\n" +
			"there is no per-VM metrics api; `fuse dashboard` shows the same data in a\n" +
			"browser alongside hosts, environments and snapshots.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cur, err := app.cfg.Current(app.ctxName)
			if err != nil {
				return err
			}
			body, err := scrapeOrchestratorMetrics(cmd.Context(), cur)
			if err != nil {
				return err
			}
			fmt.Print(string(body))
			return nil
		},
	}
}

// scrapeOrchestratorMetrics fetches the raw exposition from the context's
// orchestrator. the token is sent even though /metrics does not require one.
func scrapeOrchestratorMetrics(ctx context.Context, cur *config.Context) ([]byte, error) {
	base, err := url.Parse(cur.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base url %q: %w", cur.BaseURL, err)
	}
	ref, _ := url.Parse("/metrics")
	u := base.ResolveReference(ref)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if cur.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cur.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	// map auth/forbidden/etc through the same friendly mapping as the rest
	// of the cli (CheckResponse returns nil for 2xx without reading body).
	if err := fuse.CheckResponse(resp); err != nil {
		return nil, friendly(err)
	}
	return io.ReadAll(resp.Body)
}
