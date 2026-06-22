package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
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
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("metrics request failed: %s\n%s", resp.Status, string(body))
			}
			fmt.Print(string(body))
			return nil
		},
	}
}
