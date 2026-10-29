package main

import (
	"fmt"
	"strings"
	"time"

	fuse "github.com/folsomintel/fuse/sdks/go"
	"github.com/spf13/cobra"
)

func newAPIKeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "apikeys",
		Aliases: []string{"apikey", "keys"},
		Short:   "Manage orchestrator API keys (master token required)",
		Long: "api keys are full-access unless minted with hitl scopes. these commands\n" +
			"require the master token, so the active context must be connected with\n" +
			"--master (a scoped key may only mint a narrower copy of itself).",
	}
	cmd.AddCommand(newAPIKeyCreateCmd(), newAPIKeyListCmd(), newAPIKeyRevokeCmd())
	return cmd
}

func newAPIKeyCreateCmd() *cobra.Command {
	var (
		label, scopes, env, actor, actorName string
		ttl                                  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an API key (the secret is shown once)",
		Long: "without --scopes the key is full-access, like every key before scopes\n" +
			"existed. with --scopes it can only use the hitl routes and must expire\n" +
			"(--ttl, at most 24h); --env limits it to one environment and its forks and\n" +
			"--actor pins who its requests are recorded as. `fuse hitl invite` is the\n" +
			"short form for one environment.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			opts := fuse.CreateAPIKeyOptions{Label: label, Scopes: parseScopes(scopes), EnvID: env, TTL: ttl}
			if actor != "" {
				opts.Actor = &fuse.ActorLabel{ID: actor, Name: actorName}
			}
			key, err := cl.APIKeys.CreateWithOptions(cmd.Context(), opts)
			if err != nil {
				return friendly(err)
			}
			return renderCreatedKey(key)
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "human-readable label for the key")
	cmd.Flags().StringVar(&scopes, "scopes", "", "comma-separated hitl scopes: read, decide, secret, takeover, act-as (omit for full access)")
	cmd.Flags().StringVar(&env, "env", "", "limit a scoped key to this environment and its forks")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "expire the key after this long (required with --scopes, at most 24h)")
	cmd.Flags().StringVar(&actor, "actor", "", "pin the actor every request on the key is recorded as")
	cmd.Flags().StringVar(&actorName, "actor-name", "", "display name for --actor")
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
				scopes := "full access"
				if k.Scopes != nil {
					scopes = strings.Join(k.Scopes, ",")
				}
				expires := "-"
				if k.ExpiresAt != nil {
					expires = shortTime(*k.ExpiresAt)
				}
				actor := "-"
				if k.Actor != nil {
					actor = k.Actor.ID
				}
				rows = append(rows, []string{k.ID, dash(k.Label), scopes, dash(k.EnvID), actor, shortTime(k.CreatedAt), lastUsed, expires, revoked})
			}
			renderTable([]string{"ID", "LABEL", "SCOPES", "ENV", "ACTOR", "CREATED", "LAST USED", "EXPIRES", "REVOKED"}, rows)
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
