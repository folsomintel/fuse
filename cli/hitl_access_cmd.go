package main

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
	fuse "github.com/folsomintel/fuse/sdks/go"
	"github.com/spf13/cobra"
)

// addActorFlag adds --actor and --actor-name, sent as the Fuse-Actor
// headers. the master token may name anyone; a key needs hitl:act-as.
func addActorFlag(cmd *cobra.Command, actor *string) {
	cmd.Flags().StringVar(actor, "actor", "", "record this decision as this actor id (master token, or a key with hitl:act-as)")
}

// hitlClient is the active client, acting as actor when one is named.
func hitlClient(actor string) (*fuse.Client, error) {
	cl, _, err := app.client()
	if err != nil {
		return nil, err
	}
	if actor != "" {
		cl = cl.WithActor(fuse.ActorLabel{ID: actor})
	}
	return cl, nil
}

// renderActor is "alice (pinned)" or "alice (asserted by ak_1)".
func renderActor(a fuse.Actor) string {
	switch {
	case a.Source == "asserted" && a.KeyID != "":
		return a.ID + " (asserted by " + a.KeyID + ")"
	case a.Source == "" || a.Source == "key":
		return a.ID
	}
	return a.ID + " (" + a.Source + ")"
}

// parseScopes accepts "decide" as well as "hitl:decide".
func parseScopes(raw string) []string {
	var out []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, ":") {
			s = "hitl:" + s
		}
		out = append(out, s)
	}
	return out
}

func newHitlInviteCmd() *cobra.Command {
	var (
		scopes, actor, actorName, label string
		ttl                             time.Duration
	)
	cmd := &cobra.Command{
		Use:   "invite <env>",
		Short: "Mint a short-lived key for someone to handle one environment's requests",
		Long: "mints a key scoped to one environment (and every environment forked from\n" +
			"it) that can only use the hitl routes. --actor pins who every request on\n" +
			"the key is recorded as, so the audit log names them. the key expires after\n" +
			"--ttl (at most 24h) and is revoked when the environment is destroyed.\n\n" +
			"the secret is printed once.",
		Example: "  fuse hitl invite fuse-task-1 --scopes decide --ttl 2h --actor alice",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			opts := fuse.CreateAPIKeyOptions{
				Label:  label,
				Scopes: parseScopes(scopes),
				EnvID:  args[0],
				TTL:    ttl,
			}
			if opts.Label == "" {
				opts.Label = "invite " + args[0]
			}
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
	cmd.Flags().StringVar(&scopes, "scopes", "decide", "comma-separated: read, decide, secret, takeover, act-as")
	cmd.Flags().DurationVar(&ttl, "ttl", time.Hour, "how long the key works, at most 24h")
	cmd.Flags().StringVar(&actor, "actor", "", "pin the actor every request on the key is recorded as")
	cmd.Flags().StringVar(&actorName, "actor-name", "", "display name for --actor")
	cmd.Flags().StringVar(&label, "label", "", "label for the key (default \"invite <env>\")")
	return cmd
}

func newHitlAuditCmd() *cobra.Command {
	var (
		env, actor, since string
		verify            bool
	)
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Read the hitl audit log, or --verify its hash chain",
		Long: "lists every raise, decision, expiry, cancel and invite, with who did it\n" +
			"and from where (never what was asked). each row carries the hash of the one\n" +
			"before it; --verify reads the whole log and reports the first row that was\n" +
			"edited or removed. --verify reads every row, so it takes no filters and\n" +
			"needs a key that is not scoped to one environment.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if verify && (env != "" || actor != "" || since != "") {
				return fmt.Errorf("--verify reads the whole log; drop --env, --actor and --since")
			}
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			rows, err := cl.Interventions.AuditAll(cmd.Context(), fuse.AuditOptions{EnvID: env, Actor: actor, Since: since})
			if err != nil {
				return friendly(err)
			}
			if verify {
				return verifyAudit(rows)
			}
			if app.isJSON() {
				return printJSON(rows)
			}
			if len(rows) == 0 {
				fmt.Println("no results")
				return nil
			}
			out := make([][]string, 0, len(rows))
			for _, e := range rows {
				out = append(out, []string{fmt.Sprint(e.Seq), shortTime(e.At), e.Event, dash(e.EnvID), dash(e.InterventionID), renderActor(e.Actor), dash(e.RemoteAddr)})
			}
			renderTable([]string{"SEQ", "AT", "EVENT", "ENV", "INTERVENTION", "ACTOR", "FROM"}, out)
			return nil
		},
	}
	cmd.Flags().StringVar(&env, "env", "", "only rows for this environment")
	cmd.Flags().StringVar(&actor, "actor", "", "only rows for this actor id")
	cmd.Flags().StringVar(&since, "since", "", "only rows since this RFC 3339 time or this long ago (e.g. 24h)")
	cmd.Flags().BoolVar(&verify, "verify", false, "check the hash chain and report the first broken row")
	return cmd
}

// verifyAudit recomputes the chain client-side, so a tampered row is found
// without trusting the server's own view of it.
func verifyAudit(rows []fuse.AuditEntry) error {
	entries := make([]hitl.AuditEntry, 0, len(rows))
	for _, r := range rows {
		prev, err := hex.DecodeString(r.PrevHash)
		if err != nil {
			return fmt.Errorf("row %d: bad prev_hash: %w", r.Seq, err)
		}
		hash, err := hex.DecodeString(r.Hash)
		if err != nil {
			return fmt.Errorf("row %d: bad hash: %w", r.Seq, err)
		}
		entries = append(entries, hitl.AuditEntry{
			Seq: r.Seq, Event: hitl.AuditEvent(r.Event), InterventionID: r.InterventionID, EnvID: r.EnvID,
			Actor:      hitl.Actor{ID: r.Actor.ID, Name: r.Actor.Name, Source: hitl.ActorSource(r.Actor.Source), KeyID: r.Actor.KeyID},
			RemoteAddr: r.RemoteAddr, At: r.At, PrevHash: prev, Hash: hash,
		})
	}
	if err := hitl.VerifyChain(entries); err != nil {
		return err
	}
	if app.isJSON() {
		return printJSON(map[string]any{"ok": true, "rows": len(entries)})
	}
	successf("audit chain intact (%d rows)", len(entries))
	return nil
}

// renderCreatedKey prints a freshly minted key, warning that the secret is
// shown once.
func renderCreatedKey(key *fuse.CreatedAPIKey) error {
	// warn before the output-mode branch so json/scripted callers are
	// warned too (warnf goes to stderr, leaving json stdout clean).
	warnf("save this key now: the secret is shown only once and cannot be recovered")
	if app.isJSON() {
		return printJSON(key)
	}
	pairs := [][2]string{
		{"id", key.ID},
		{"label", dash(key.Label)},
		{"created", shortTime(key.CreatedAt)},
	}
	if key.Scopes != nil {
		pairs = append(pairs, [2]string{"scopes", strings.Join(key.Scopes, ", ")})
	}
	if key.EnvID != "" {
		pairs = append(pairs, [2]string{"env", key.EnvID + " (and its forks)"})
	}
	if key.ExpiresAt != nil {
		pairs = append(pairs, [2]string{"expires", shortTime(*key.ExpiresAt)})
	}
	if key.Actor != nil {
		pairs = append(pairs, [2]string{"actor", key.Actor.ID})
	}
	pairs = append(pairs, [2]string{"key", styleGood.Render(key.Key)})
	renderDetail(pairs)
	return nil
}
