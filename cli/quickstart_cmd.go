package main

import (
	"fmt"
	"net/url"
	"strconv"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/folsomintel/fuse/cli/config"
	fuse "github.com/folsomintel/fuse/sdks/go"
)

// newQuickstartCmd wraps the two-step bring-up (connect an orchestrator,
// then register a host) that is otherwise undiscoverable. It prompts,
// verifies each hop as it goes, and leaves the operator with a selected
// host ready for `environment create`.
func newQuickstartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "quickstart",
		Short: "Guided first-time setup: connect an orchestrator and register a host",
		Long: "quickstart walks first-time setup end to end: it connects to an\n" +
			"orchestrator (verifying the url and token), registers a compute host\n" +
			"(verifying the host agent token), and selects that host. it is the\n" +
			"discoverable front door for `fuse connect` + `fuse host register`.\n\n" +
			"the two tokens are different: the orchestrator token (ORCH_AUTH_TOKEN)\n" +
			"authenticates you to the orchestrator; the host agent token\n" +
			"(FC_AGENT_TOKEN) is what the orchestrator uses to reach the host.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !isInteractive() {
				return fmt.Errorf("quickstart is interactive; on a non-tty use `fuse connect <url> --token ...` then `fuse host register <id> --url ... --token ...`")
			}

			// Step 1: connect an orchestrator.
			var (
				orchURL   string
				orchToken string
				master    bool
			)
			if err := runForm(huh.NewGroup(
				huh.NewInput().Title("Orchestrator URL").Description("scheme://host:port (default port 8080)").Value(&orchURL),
				huh.NewInput().Title("Orchestrator token").Description("ORCH_AUTH_TOKEN (empty for an insecure/dev orchestrator)").EchoMode(huh.EchoModePassword).Value(&orchToken),
				huh.NewConfirm().Title("Is this the master token?").Description("needed for api-key commands").Value(&master),
			)); err != nil {
				return err
			}
			if orchURL == "" {
				return fmt.Errorf("orchestrator url is required")
			}
			infof("verifying orchestrator at %s ...", orchURL)
			if err := probeOrchestrator(cmd.Context(), orchURL, orchToken); err != nil {
				return err
			}
			name := contextNameFor(orchURL)
			app.cfg.Add(config.Context{
				Name:    name,
				BaseURL: orchURL,
				Token:   orchToken,
				Master:  master,
			})
			if err := app.cfg.Save(); err != nil {
				return err
			}
			successf("connected: context %q -> %s", name, orchURL)

			// Step 2: register a host.
			var (
				hostID    string
				hostURL   string
				hostToken string
				region    string
				maxVMsS   = "0"
			)
			if err := runForm(huh.NewGroup(
				huh.NewInput().Title("Host ID").Description("a readable id, e.g. prod-east-1").Value(&hostID),
				huh.NewInput().Title("Host agent URL").Description("scheme://host:port (default port 8090)").Value(&hostURL),
				huh.NewInput().Title("Host agent token").Description("the agent's FC_AGENT_TOKEN").EchoMode(huh.EchoModePassword).Value(&hostToken),
				huh.NewInput().Title("Region").Value(&region),
				huh.NewInput().Title("Max VMs").Description("scheduling cap; required (cpu/ram/storage are probed from the agent)").Value(&maxVMsS).Validate(validateInt),
			)); err != nil {
				return err
			}
			if hostID == "" || hostURL == "" {
				return fmt.Errorf("host id and host agent url are required")
			}
			if hostToken == "" {
				return fmt.Errorf("host agent token is required (the agent's FC_AGENT_TOKEN)")
			}
			maxVMs, _ := strconv.Atoi(maxVMsS)
			if maxVMs <= 0 {
				return fmt.Errorf("max vms must be a positive number")
			}

			cl, err := fuse.New(orchURL, orchToken, fuse.WithUserAgent("fuse-cli/"+version))
			if err != nil {
				return err
			}
			infof("registering host %q (probing the agent for capacity) ...", hostID)
			h, err := cl.Hosts.Register(cmd.Context(), fuse.RegisterHostRequest{
				ID:       hostID,
				URL:      hostURL,
				Token:    hostToken,
				Region:   region,
				Capacity: fuse.HostCapacity{VMCount: maxVMs},
			})
			if err != nil {
				return friendly(err)
			}
			// Target the context we just created explicitly, so a --context
			// override elsewhere can't redirect the selection.
			if err := app.cfg.SetActiveHost(name, hostID); err != nil {
				return err
			}
			if err := app.cfg.Save(); err != nil {
				return err
			}
			successf("registered host %q and selected it", hostID)
			renderHostDetail(h)
			infof("you're set up. next: fuse environment create --name demo")
			return nil
		},
	}
}

// contextNameFor derives a context name from an orchestrator url, falling
// back to "default" when the host cannot be parsed out.
func contextNameFor(rawURL string) string {
	if parsed, err := url.Parse(rawURL); err == nil {
		if h := parsed.Hostname(); h != "" {
			return h
		}
	}
	return "default"
}
