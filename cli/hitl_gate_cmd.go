package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

func newHitlApproveCmd() *cobra.Command {
	var (
		reason, actor, grant string
		edit                 bool
		extend               time.Duration
	)
	cmd := &cobra.Command{
		Use:   "approve <id> [--edit [-- <argv>...]] [--grant env|ttl:30m]",
		Short: "Approve a held exec or lifecycle action",
		Long: "approves an exec or lifecycle action a hitl gate is holding. the\n" +
			"orchestrator then performs it and the waiting agent gets the result.\n\n" +
			"--edit replaces a held exec's argv before it runs. pass the new argv after\n" +
			"`--`, or give no argv to edit the current one in $EDITOR, one argument per\n" +
			"line. the edit is re-checked against the deny gates.\n\n" +
			"    fuse hitl approve int_1 --edit -- git push --force-with-lease\n\n" +
			"--grant lets identical repeats through without asking again: env for the\n" +
			"environment's lifetime, or ttl:<duration>. a grant cannot be combined with\n" +
			"an edit.\n\n" +
			"for an ask, `fuse hitl approve <id> <answer>` still answers it. a stall\n" +
			"signal is acked with a plain approve; an expiry takes --extend for how long\n" +
			"to keep the environment alive (default: one more window of its reaper).",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			before, after := args, []string(nil)
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				before, after = args[:dash], args[dash:]
			}
			if len(before) == 0 {
				return errors.New("intervention id is required")
			}
			id := before[0]
			if len(after) > 0 && !edit {
				return errors.New("an argv after `--` needs --edit")
			}
			cl, err := hitlClient(actor)
			if err != nil {
				return err
			}
			opts := fuse.ApproveOptions{Reason: reason, Grant: grant, Answer: strings.Join(before[1:], " "), ExtendSeconds: int64(extend / time.Second)}
			if edit {
				opts.EditedArgv = after
				if len(opts.EditedArgv) == 0 {
					in, err := cl.Interventions.Get(cmd.Context(), id)
					if err != nil {
						return friendly(err)
					}
					if in.Payload.Exec == nil {
						return fmt.Errorf("%s is not a held exec; only an exec can be edited", id)
					}
					if opts.EditedArgv, err = editArgv(in.Payload.Exec.Argv); err != nil {
						return err
					}
				}
			}
			in, err := cl.Interventions.ApproveWith(cmd.Context(), id, opts)
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(in)
			}
			msg := "approved " + in.ID
			if in.Decision != nil && in.Decision.EditedArgv != nil {
				msg += ", running " + renderArgv(in.Decision.EditedArgv)
			}
			if in.Decision != nil && in.Decision.Grant != "" && in.Decision.Grant != "once" {
				msg += " (grant " + in.Decision.Grant + ")"
			}
			if s := in.Payload.Signal; s != nil && s.ExtendedUntil != nil {
				msg += ", environment kept until " + shortTime(*s.ExtendedUntil)
			}
			successf("%s", msg)
			return nil
		},
	}
	cmd.Flags().BoolVar(&edit, "edit", false, "replace the held exec's argv (after `--`, or in $EDITOR)")
	cmd.Flags().StringVar(&grant, "grant", "", "let identical repeats through: env or ttl:<duration> (default: the gate's)")
	cmd.Flags().StringVar(&reason, "reason", "", "optional note passed back to the agent")
	cmd.Flags().DurationVar(&extend, "extend", 0, "for an expiry: keep the environment alive this long, like 30m")
	addActorFlag(cmd, &actor)
	return cmd
}

// editArgv opens argv in $EDITOR, one argument per line, and reads it back.
// blank lines are dropped.
func editArgv(argv []string) ([]string, error) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		return nil, errors.New("set $EDITOR, or pass the new argv after `--`")
	}
	f, err := os.CreateTemp("", "fuse-argv-*.txt")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(strings.Join(argv, "\n") + "\n"); err != nil {
		_ = f.Close()
		return nil, err
	}
	_ = f.Close()

	// $EDITOR may carry flags, like "code --wait".
	parts := strings.Fields(editor)
	c := exec.Command(parts[0], append(parts[1:], f.Name())...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("editor: %w", err)
	}

	b, err := os.ReadFile(f.Name())
	if err != nil {
		return nil, err
	}
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		if line := sc.Text(); strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the edited argv is empty; nothing approved")
	}
	return out, nil
}

// printHeld tells the person at the terminal that a gate is holding their
// call and how to decide it. it goes to stderr so stdout stays the result.
func printHeld(in *fuse.Intervention) {
	what := in.Kind
	switch {
	case in.Payload.Exec != nil:
		what = "exec " + renderArgv(in.Payload.Exec.Argv)
	case in.Payload.Lifecycle != nil:
		what = in.Payload.Lifecycle.Action
	}
	fmt.Fprintf(os.Stderr, "held by hitl gate %s: waiting on intervention %s (%s)\n", in.Gate, in.ID, what)
	fmt.Fprintf(os.Stderr, "  decide with: fuse hitl approve %s   or   fuse hitl reject %s --reason ...\n", in.ID, in.ID)
}

// renderArgv joins argv for display, quoting arguments that need it.
func renderArgv(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`") {
			a = strconv.Quote(a)
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}
