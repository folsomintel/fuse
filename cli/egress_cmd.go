package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newEnvEgressCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "egress",
		Short: "Change a running environment's outbound traffic policy",
	}
	cmd.AddCommand(newEnvEgressAllowCmd())
	return cmd
}

func newEnvEgressAllowCmd() *cobra.Command {
	var lift bool
	cmd := &cobra.Command{
		Use:   "allow <id> [CIDR...]",
		Short: "Replace a running environment's outbound allowlist",
		Long: "allow replaces a running direct-egress environment's outbound ipv4 allowlist.\n" +
			"each CIDR is an ipv4 cidr or a bare address (a /32); at most 256. the list\n" +
			"replaces the previous one, and connections it no longer allows are cut. dns\n" +
			"to the guest's resolver stays allowed. --clear lifts the restriction; with no\n" +
			"CIDRs and no --clear the command refuses, so a typo cannot lift it.\n" +
			"proxy-egress environments (warp, wireguard) refuse an allowlist.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, cidrs := args[0], args[1:]
			if lift && len(cidrs) > 0 {
				return fmt.Errorf("--clear lifts the allowlist; pass either --clear or CIDRs, not both")
			}
			if !lift && len(cidrs) == 0 {
				return fmt.Errorf("no CIDRs given: pass one or more CIDRs, or --clear to lift the allowlist")
			}
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			st, err := cl.Environments.SetEgressAllow(cmd.Context(), id, cidrs)
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(st)
			}
			if len(st.Allow) == 0 {
				successf("lifted the egress allowlist of environment %q", id)
				return nil
			}
			successf("environment %q may reach %s", id, strings.Join(st.Allow, ", "))
			return nil
		},
	}
	cmd.Flags().BoolVar(&lift, "clear", false, "lift the allowlist (unrestricted direct egress)")
	return cmd
}
