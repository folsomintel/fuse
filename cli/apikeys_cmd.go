package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newAPIKeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "apikeys",
		Aliases: []string{"apikey", "keys"},
		Short:   "Manage orchestrator-global API keys (master token required)",
		Long: "api keys are orchestrator-global and all-or-nothing. these commands require\n" +
			"the master token, so the active context must be connected with --master.",
	}
	cmd.AddCommand(newAPIKeyCreateCmd(), newAPIKeyListCmd(), newAPIKeyRevokeCmd())
	return cmd
}

func newAPIKeyCreateCmd() *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an API key (the secret is shown once)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			key, err := cl.APIKeys.Create(cmd.Context(), label)
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(key)
			}
			renderDetail([][2]string{
				{"id", key.ID},
				{"label", dash(key.Label)},
				{"created", shortTime(key.CreatedAt)},
				{"key", styleGood.Render(key.Key)},
			})
			warnf("save this key now: the secret is shown only once and cannot be recovered")
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "human-readable label for the key")
	return cmd
}

func newAPIKeyListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List API keys (metadata only)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			keys, err := cl.APIKeys.List(cmd.Context())
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(keys)
			}
			rows := make([][]string, 0, len(keys))
			for _, k := range keys {
				lastUsed := "-"
				if k.LastUsedAt != nil {
					lastUsed = shortTime(*k.LastUsedAt)
				}
				revoked := "-"
				if k.RevokedAt != nil {
					revoked = shortTime(*k.RevokedAt)
				}
				rows = append(rows, []string{k.ID, dash(k.Label), shortTime(k.CreatedAt), lastUsed, revoked})
			}
			renderTable([]string{"ID", "LABEL", "CREATED", "LAST USED", "REVOKED"}, rows)
			return nil
		},
	}
}

func newAPIKeyRevokeCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke an API key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if !yes {
				ok, err := confirm(fmt.Sprintf("Revoke API key %q?", id))
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("aborted (use --yes to skip confirmation)")
				}
			}
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			if err := cl.APIKeys.Revoke(cmd.Context(), id); err != nil {
				return friendly(err)
			}
			successf("revoked API key %q", id)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}
