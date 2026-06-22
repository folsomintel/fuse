package main

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/andrewn6/fuse/cli/config"
)

func newConnectCmd() *cobra.Command {
	var (
		token  string
		name   string
		master bool
	)
	cmd := &cobra.Command{
		Use:   "connect <url>",
		Short: "Add an orchestrator context and make it active",
		Long: "connect stores an orchestrator's base url and bearer token as a named\n" +
			"context and makes it the active one. the url is the orchestrator root\n" +
			"(scheme://host:port), without a /v1 suffix.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			rawURL := args[0]
			parsed, err := url.Parse(rawURL)
			if err != nil || parsed.Host == "" {
				return fmt.Errorf("invalid url %q: want scheme://host[:port]", rawURL)
			}
			if name == "" {
				name = parsed.Hostname()
			}
			if name == "" {
				return fmt.Errorf("could not derive a context name from %q: pass --name", rawURL)
			}
			app.cfg.Add(config.Context{
				Name:    name,
				BaseURL: rawURL,
				Token:   token,
				Master:  master,
			})
			if err := app.cfg.Save(); err != nil {
				return err
			}
			if token == "" {
				warnf("no token set; this only works against an orchestrator in insecure/dev mode")
			}
			successf("connected: context %q -> %s", name, rawURL)
			return nil
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "bearer token for the orchestrator")
	cmd.Flags().StringVar(&name, "name", "", "context name (default: host from url)")
	cmd.Flags().BoolVar(&master, "master", false, "mark this token as the master token (needed for api-key commands)")
	return cmd
}

func newContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "context",
		Aliases: []string{"contexts", "ctx"},
		Short:   "Manage orchestrator contexts",
	}
	cmd.AddCommand(newContextListCmd(), newContextUseCmd(), newContextRemoveCmd(), newContextCurrentCmd())
	return cmd
}

// contextView is the sanitized json shape (never includes the token).
type contextView struct {
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	Master     bool   `json:"master"`
	ActiveHost string `json:"active_host,omitempty"`
	Current    bool   `json:"current"`
}

func newContextListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured contexts",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			views := make([]contextView, 0, len(app.cfg.Contexts))
			for _, c := range app.cfg.Contexts {
				views = append(views, contextView{
					Name:       c.Name,
					BaseURL:    c.BaseURL,
					Master:     c.Master,
					ActiveHost: c.ActiveHost,
					Current:    c.Name == app.cfg.CurrentContext,
				})
			}
			if app.isJSON() {
				return printJSON(views)
			}
			rows := make([][]string, 0, len(views))
			for _, v := range views {
				cur := ""
				if v.Current {
					cur = "*"
				}
				master := ""
				if v.Master {
					master = "yes"
				}
				rows = append(rows, []string{cur, v.Name, v.BaseURL, master, dash(v.ActiveHost)})
			}
			renderTable([]string{"", "NAME", "BASE URL", "MASTER", "ACTIVE HOST"}, rows)
			return nil
		},
	}
}

func newContextUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Switch the active context",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := app.cfg.Use(args[0]); err != nil {
				return err
			}
			if err := app.cfg.Save(); err != nil {
				return err
			}
			successf("now using context %q", args[0])
			return nil
		},
	}
}

func newContextRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Remove a context",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := app.cfg.Remove(args[0]); err != nil {
				return err
			}
			if err := app.cfg.Save(); err != nil {
				return err
			}
			successf("removed context %q", args[0])
			return nil
		},
	}
}

func newContextCurrentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "current",
		Short: "Show the active context",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cur, err := app.cfg.Current(app.ctxName)
			if err != nil {
				return err
			}
			if app.isJSON() {
				return printJSON(contextView{
					Name:       cur.Name,
					BaseURL:    cur.BaseURL,
					Master:     cur.Master,
					ActiveHost: cur.ActiveHost,
					Current:    true,
				})
			}
			renderDetail([][2]string{
				{"name", cur.Name},
				{"base url", cur.BaseURL},
				{"master", fmt.Sprintf("%t", cur.Master)},
				{"active host", dash(cur.ActiveHost)},
				{"config", app.cfg.Path()},
			})
			return nil
		},
	}
}
