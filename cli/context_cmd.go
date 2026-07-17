package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	fuse "github.com/folsomintel/fuse/sdks/go"

	"github.com/folsomintel/fuse/cli/config"
)

func newConnectCmd() *cobra.Command {
	var (
		token    string
		name     string
		master   bool
		noVerify bool
	)
	cmd := &cobra.Command{
		Use:   "connect <url>",
		Short: "Add an orchestrator context and make it active",
		Long: "connect stores an orchestrator's base url and bearer token as a named\n" +
			"context and makes it the active one. the url is the orchestrator root\n" +
			"(scheme://host:port), without a /v1 suffix.\n\n" +
			"before saving, connect probes the url to confirm it really is a fuse\n" +
			"orchestrator (and that the token works), so a wrong port, a wrong\n" +
			"service, or a wrong token fails here with a clear message instead of\n" +
			"one command later. pass --no-verify to skip the probe for offline or\n" +
			"scripted setup.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			if !noVerify {
				if err := probeOrchestrator(cmd.Context(), rawURL, token); err != nil {
					return err
				}
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
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "skip the pre-save probe (offline/scripted setup)")
	return cmd
}

// probeOrchestrator confirms that baseURL is a reachable fuse orchestrator
// and, if a token is supplied, that the orchestrator accepts it. It maps
// each failure to a message that names the layer that failed -- not
// running, wrong service/port, or wrong token -- so a misconfiguration is
// caught at connect time rather than surfacing one command later as an
// unrelated-looking error. Shared by `connect` and `quickstart`.
func probeOrchestrator(ctx context.Context, baseURL, token string) error {
	cl, err := fuse.New(baseURL, token, fuse.WithUserAgent("fuse-cli/"+version))
	if err != nil {
		return err
	}
	info, err := cl.Version(ctx)
	if err != nil {
		return fmt.Errorf("no orchestrator at %s (%s). is it running? the orchestrator default port is 8080. pass --no-verify to skip this check",
			baseURL, dialErrReason(err))
	}
	if !info.IsOrchestrator() {
		return fmt.Errorf("%s is not a fuse orchestrator (%s). the orchestrator default port is 8080; 8090 is the host agent",
			baseURL, gotService(info))
	}
	// Identity confirmed. Verify the token only if one was supplied: a
	// 401 means the orchestrator rejected it; a 403 (valid token, wrong
	// tier) or any other error does not mean the token is wrong, so we
	// accept it and let the specific command report the real problem.
	if token != "" {
		if _, err := cl.Hosts.List(ctx); err != nil && fuse.IsUnauthorized(err) {
			return fmt.Errorf("orchestrator at %s rejected the token. this must be ORCH_AUTH_TOKEN, not the host-agent token", baseURL)
		}
	}
	return nil
}

// gotService describes the wrong thing a probe actually reached, using the
// Server header when present (e.g. "got Server: fc-agent/0.1") and falling
// back to the service field it self-reported.
func gotService(info *fuse.VersionInfo) string {
	if info == nil {
		return "no response body"
	}
	if info.ServerHeader != "" {
		return "got Server: " + info.ServerHeader
	}
	if info.Service != "" {
		return fmt.Sprintf("got service %q", info.Service)
	}
	return fmt.Sprintf("got HTTP %d with no fuse identity", info.StatusCode)
}

// dialErrReason reduces a transport error to a short human phrase so the
// connect message reads cleanly instead of echoing the full net/url error.
func dialErrReason(err error) string {
	if err == nil {
		return "unknown error"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	case strings.Contains(msg, "no such host"):
		return "host not found"
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return "timed out"
	case strings.Contains(msg, "no route to host"):
		return "no route to host"
	default:
		return msg
	}
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
