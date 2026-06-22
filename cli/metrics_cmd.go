package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	fuse "github.com/andrewn6/fuse/sdks/go"
)

func newMetricsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "metrics",
		Short: "Print fleet-wide Prometheus metrics from the orchestrator",
		Long: "metrics scrapes the orchestrator's /metrics endpoint and prints the raw\n" +
			"Prometheus exposition (fleet-wide aggregates). there is no per-VM metrics\n" +
			"api; for per-host capacity use `fuse host metrics <id>`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cur, err := app.cfg.Current(app.ctxName)
			if err != nil {
				return err
			}
			base, err := url.Parse(cur.BaseURL)
			if err != nil {
				return fmt.Errorf("invalid base url %q: %w", cur.BaseURL, err)
			}
			ref, _ := url.Parse("/metrics")
			u := base.ResolveReference(ref)

			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, u.String(), nil)
			if err != nil {
				return err
			}
			if cur.Token != "" {
				req.Header.Set("Authorization", "Bearer "+cur.Token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			// map auth/forbidden/etc through the same friendly mapping as the rest
			// of the cli (CheckResponse returns nil for 2xx without reading body).
			if err := fuse.CheckResponse(resp); err != nil {
				return friendly(err)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			fmt.Print(string(body))
			return nil
		},
	}
}
