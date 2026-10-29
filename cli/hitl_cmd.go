package main

import (
	"fmt"
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
			"deciding requires the master token.",
	}
	cmd.AddCommand(newHitlListCmd(), newHitlShowCmd(), newHitlAnswerCmd(), newHitlRejectCmd())
	return cmd
}

func newHitlListCmd() *cobra.Command {
	var env, state string
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
			list, err := cl.Interventions.List(cmd.Context(), fuse.ListInterventionsOptions{EnvID: env, State: state})
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
	var reason string
	cmd := &cobra.Command{
		Use:     "answer <id> <answer>",
		Aliases: []string{"approve"},
		Short:   "Answer an ask (approves it)",
		Long: "answers a pending ask, which approves it and hands the answer back to\n" +
			"the waiting agent. when the ask offered choices, the answer must be one\n" +
			"of them. everything after the id is joined into the answer.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
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
	return cmd
}

func newHitlRejectCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "reject <id>",
		Short: "Reject an intervention",
		Long: "rejects a pending intervention. the reason is handed back to the agent so\n" +
			"it can replan; give it one.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
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
		{"question", in.Payload.Question},
	}
	if len(in.Payload.Choices) > 0 {
		pairs = append(pairs, [2]string{"choices", strings.Join(in.Payload.Choices, ", ")})
	}
	pairs = append(pairs,
		[2]string{"raised by", in.TriggeredBy.ID},
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
		pairs = append(pairs,
			[2]string{"decided by", d.DecidedBy.ID},
			[2]string{"decided", shortTime(d.DecidedAt)},
		)
	}
	renderDetail(pairs)
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
