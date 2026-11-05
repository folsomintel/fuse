package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	fuse "github.com/folsomintel/fuse/sdks/go"
	"github.com/spf13/cobra"
)

func newHitlCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "hitl",
		Aliases: []string{"interventions"},
		Short:   "See and answer human-in-the-loop requests",
		Long: "an agent running against an environment with a `hitl:` block can ask a\n" +
			"human for something. these commands list those requests and answer them.\n" +
			"deciding needs the master token or a key with hitl:decide (see `fuse hitl\n" +
			"invite`), and never the key or actor that raised the request.",
	}
	cmd.AddCommand(newHitlListCmd(), newHitlShowCmd(), newHitlAnswerCmd(), newHitlApproveCmd(), newHitlRejectCmd(),
		newHitlInviteCmd(), newHitlAuditCmd(), newHitlSecretCmd(), newHitlTakeoverCmd(), newHitlReleaseCmd())
	return cmd
}

func newHitlListCmd() *cobra.Command {
	var env, state, actor string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List interventions (pending by default)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if state == "all" {
				state = ""
			}
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			list, err := cl.Interventions.List(cmd.Context(), fuse.ListInterventionsOptions{EnvID: env, State: state, Actor: actor})
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(list)
			}
			if len(list) == 0 {
				fmt.Println("no results")
				return nil
			}
			rows := make([][]string, 0, len(list))
			for _, in := range list {
				rows = append(rows, []string{in.ID, in.EnvID, in.Kind, renderInterventionState(in), truncate(in.Payload.Question, 60), untilOrAgo(in)})
			}
			renderTable([]string{"ID", "ENV", "KIND", "STATE", "QUESTION", "DEADLINE"}, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&env, "env", "", "only interventions for this environment")
	cmd.Flags().StringVar(&state, "state", fuse.InterventionPending, "pending, approved, rejected, expired, cancelled, or all")
	cmd.Flags().StringVar(&actor, "actor", "", "only interventions raised or decided by this actor id")
	return cmd
}

func newHitlShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one intervention",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			in, err := cl.Interventions.Get(cmd.Context(), args[0])
			if err != nil {
				return friendly(err)
			}
			return renderIntervention(in)
		},
	}
}

func newHitlAnswerCmd() *cobra.Command {
	var reason, actor string
	cmd := &cobra.Command{
		Use:   "answer <id> <answer>",
		Short: "Answer an ask (approves it)",
		Long: "answers a pending ask, which approves it and hands the answer back to\n" +
			"the waiting agent. when the ask offered choices, the answer must be one\n" +
			"of them. everything after the id is joined into the answer.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := hitlClient(actor)
			if err != nil {
				return err
			}
			in, err := cl.Interventions.Approve(cmd.Context(), args[0], strings.Join(args[1:], " "), reason)
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(in)
			}
			successf("answered %s", in.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "optional note passed back to the agent")
	addActorFlag(cmd, &actor)
	return cmd
}

func newHitlRejectCmd() *cobra.Command {
	var reason, actor string
	cmd := &cobra.Command{
		Use:   "reject <id>",
		Short: "Reject an intervention",
		Long: "rejects a pending intervention. the reason is handed back to the agent so\n" +
			"it can replan; give it one.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := hitlClient(actor)
			if err != nil {
				return err
			}
			in, err := cl.Interventions.Reject(cmd.Context(), args[0], reason)
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(in)
			}
			successf("rejected %s", in.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, fed back to the agent")
	addActorFlag(cmd, &actor)
	return cmd
}

func renderIntervention(in *fuse.Intervention) error {
	if app.isJSON() {
		return printJSON(in)
	}
	pairs := [][2]string{
		{"id", in.ID},
		{"env", in.EnvID},
		{"kind", in.Kind},
		{"state", renderInterventionState(*in)},
	}
	// a secret's fusefile label comes first: the agent wrote the message,
	// so it is the part a human should trust least.
	if p := in.Payload.Secret; p != nil {
		pairs = append(pairs, [2]string{"secret for", p.Label}, [2]string{"target", p.Target})
		if p.Message != "" {
			pairs = append(pairs, [2]string{"agent says", p.Message})
		}
	}
	if p := in.Payload.Takeover; p != nil {
		pairs = append(pairs, [2]string{"via", p.Via})
		if p.Reason != "" {
			pairs = append(pairs, [2]string{"reason", p.Reason})
		}
		if p.Holder != nil {
			pairs = append(pairs, [2]string{"held by", renderActor(*p.Holder)})
		}
	}
	if p := in.Payload.Signal; p != nil {
		switch {
		case p.Failures > 0:
			pairs = append(pairs, [2]string{"signal", fmt.Sprintf("%d failing execs in a row (last exit %d)", p.Failures, p.LastExitCode)})
		case p.QuietSeconds > 0:
			pairs = append(pairs, [2]string{"signal", fmt.Sprintf("no exec or computer call for %s", time.Duration(p.QuietSeconds)*time.Second)})
		case p.ReapAt != nil:
			pairs = append(pairs, [2]string{"signal", p.Reaper + " reap at " + shortTime(*p.ReapAt)})
		}
		if p.ExtendedUntil != nil {
			pairs = append(pairs, [2]string{"extended until", shortTime(*p.ExtendedUntil)})
		}
	}
	if in.Gate != "" {
		pairs = append(pairs, [2]string{"gate", in.Gate + " (" + in.Mode + ")"})
	}
	if in.Payload.Question != "" {
		pairs = append(pairs, [2]string{"question", in.Payload.Question})
	}
	if e := in.Payload.Exec; e != nil {
		pairs = append(pairs, [2]string{"argv", renderArgv(e.Argv)})
	}
	if l := in.Payload.Lifecycle; l != nil {
		pairs = append(pairs, [2]string{"action", l.Action})
	}
	if c := in.Payload.Computer; c != nil {
		pairs = append(pairs, [2]string{"action", c.Action})
		if c.Action == "type" || c.Text != "" {
			pairs = append(pairs, [2]string{"text", strconv.Quote(c.Text)})
		}
		if c.Action != "type" && len(c.Input) > 0 {
			pairs = append(pairs, [2]string{"input", string(c.Input)})
		}
	}
	if d := in.Payload.Decider; d != nil {
		note := "escalated"
		if d.Error != "" {
			note = "failed, so you were asked: " + d.Error
		} else if d.Reason != "" {
			note += ": " + d.Reason
		}
		pairs = append(pairs, [2]string{"decider", note})
	}
	if rv := in.Payload.Review; rv != nil {
		what := rv.Format
		if rv.Path != "" {
			what += " from " + rv.Path
		}
		if rv.Truncated {
			what += " (truncated)"
		}
		pairs = append(pairs, [2]string{"review", what})
		if rv.Message != "" {
			pairs = append(pairs, [2]string{"agent says", rv.Message})
		}
	}
	if len(in.Payload.Choices) > 0 {
		pairs = append(pairs, [2]string{"choices", strings.Join(in.Payload.Choices, ", ")})
	}
	pairs = append(pairs,
		[2]string{"raised by", renderActor(in.TriggeredBy)},
		[2]string{"raised", shortTime(in.CreatedAt)},
		[2]string{"deadline", shortTime(in.Deadline) + "  (" + untilOrAgo(*in) + ", then " + in.OnTimeout + ")"},
	)
	if d := in.Decision; d != nil {
		pairs = append(pairs, [2]string{"outcome", d.Outcome})
		if d.Answer != "" {
			pairs = append(pairs, [2]string{"answer", d.Answer})
		}
		if d.Reason != "" {
			pairs = append(pairs, [2]string{"reason", d.Reason})
		}
		if d.EditedArgv != nil {
			pairs = append(pairs, [2]string{"edited argv", renderArgv(d.EditedArgv)})
		}
		if d.EditedText != nil {
			pairs = append(pairs, [2]string{"edited text", strconv.Quote(*d.EditedText)})
		}
		if d.Grant != "" {
			pairs = append(pairs, [2]string{"grant", d.Grant})
		}
		pairs = append(pairs,
			[2]string{"decided by", renderActor(d.DecidedBy)},
			[2]string{"decided", shortTime(d.DecidedAt)},
		)
	}
	if r := in.Result; r != nil {
		switch {
		case r.Error != "":
			pairs = append(pairs, [2]string{"result", styleBad.Render("error: " + r.Error)})
		case r.Exec != nil:
			pairs = append(pairs, [2]string{"result", fmt.Sprintf("exit %d", r.Exec.ExitCode)})
		case r.Computer != nil:
			pairs = append(pairs, [2]string{"result", "performed"})
		case r.EnvID != "":
			pairs = append(pairs, [2]string{"result", "environment " + r.EnvID})
		case r.SnapshotID != "":
			pairs = append(pairs, [2]string{"result", "snapshot " + r.SnapshotID})
		default:
			pairs = append(pairs, [2]string{"result", "done"})
		}
	}
	renderDetail(pairs)
	// the content up for review goes under the details, in full: it is
	// what the human is deciding on.
	if rv := in.Payload.Review; rv != nil {
		fmt.Println()
		fmt.Println(rv.Content)
	}
	return nil
}

func renderInterventionState(in fuse.Intervention) string {
	switch in.State {
	case fuse.InterventionPending:
		return styleWarn.Render(in.State)
	case fuse.InterventionApproved:
		return styleGood.Render(in.State)
	case fuse.InterventionRejected, fuse.InterventionCancelled:
		return styleBad.Render(in.State)
	}
	return in.State
}

// untilOrAgo is "in 4m" for a pending intervention and "-" once resolved.
func untilOrAgo(in fuse.Intervention) string {
	if in.State != fuse.InterventionPending {
		return "-"
	}
	d := time.Until(in.Deadline).Round(time.Second)
	if d <= 0 {
		return "due"
	}
	return "in " + d.String()
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
