package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	fuse "github.com/folsomintel/fuse/sdks/go"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newHitlSecretCmd() *cobra.Command {
	var actor string
	cmd := &cobra.Command{
		Use:   "secret <id>",
		Short: "Answer a secret request",
		Long: "answers an agent's secret request. the value is read without echo (or\n" +
			"from stdin when it is not a terminal), written into the target the\n" +
			"Fusefile declares, and dropped: fuse never stores, logs or returns it, and\n" +
			"the agent only hears \"done\". needs the master token or a key with\n" +
			"hitl:secret.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := hitlClient(actor)
			if err != nil {
				return err
			}
			in, err := cl.Interventions.Get(cmd.Context(), args[0])
			if err != nil {
				return friendly(err)
			}
			if in.Kind != fuse.HitlKindSecret || in.Payload.Secret == nil {
				return fmt.Errorf("%s is a %s request, not a secret request", in.ID, in.Kind)
			}
			// the fusefile label first; the agent's text is secondary.
			p := in.Payload.Secret
			infof("secret for: %s", p.Label)
			infof("target:     %s", p.Target)
			if p.Message != "" {
				infof("agent says: %s", p.Message)
			}
			value, err := readSecretValue(p.Target)
			if err != nil {
				return err
			}
			out, err := cl.Interventions.AnswerSecret(cmd.Context(), in.ID, value)
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(out)
			}
			successf("secret delivered to %s for %s", p.Target, out.EnvID)
			return nil
		},
	}
	addActorFlag(cmd, &actor)
	return cmd
}

// readSecretValue reads the value without echo from a terminal, or the
// whole of stdin otherwise (one trailing newline dropped).
func readSecretValue(target string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprintf(os.Stderr, "value for %s (not echoed): ", target)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("read value: %w", err)
		}
		if len(b) == 0 {
			return "", errors.New("no value entered")
		}
		return string(b), nil
	}
	b, err := io.ReadAll(bufio.NewReader(os.Stdin))
	if err != nil {
		return "", fmt.Errorf("read value from stdin: %w", err)
	}
	v := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if v == "" {
		return "", errors.New("no value on stdin")
	}
	return v, nil
}

func newHitlTakeoverCmd() *cobra.Command {
	var actor, reason, via, listen string
	var noShell, noOpen bool
	cmd := &cobra.Command{
		Use:   "takeover <env>",
		Short: "Take over an environment from its agent",
		Long: "takes control of an environment and opens your own shell in it. it claims\n" +
			"the agent's pending takeover request if there is one. while you hold it,\n" +
			"the agent's exec and computer calls wait. the shell is recorded; ask for\n" +
			"credentials through a secret request instead of typing them here.\n" +
			"leaving the shell keeps the takeover held; end it with `fuse hitl release`.\n\n" +
			"--via desktop opens the live desktop in the browser instead, like `fuse\n" +
			"desktop`. it is recorded as an input log (clicks and named keys; typed\n" +
			"text is counted, never kept) plus a screenshot every few seconds.\n" +
			"needs the master token or a key with hitl:takeover.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := hitlClient(actor)
			if err != nil {
				return err
			}
			if via != fuse.TakeoverViaTerminal && via != fuse.TakeoverViaDesktop {
				return fmt.Errorf("--via must be terminal or desktop, got %q", via)
			}
			in, err := cl.Environments.Takeover(cmd.Context(), args[0], fuse.TakeoverOptions{Via: via, Reason: reason})
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() || noShell {
				if app.isJSON() {
					return printJSON(in)
				}
				successf("holding %s (takeover %s, until %s)", in.EnvID, in.ID, shortTime(in.Deadline))
				return nil
			}
			if via == fuse.TakeoverViaDesktop {
				infof("holding %s (takeover %s, until %s). the agent is paused; this view is recorded.", in.EnvID, in.ID, shortTime(in.Deadline))
				open := func(ctx context.Context) (io.ReadWriteCloser, error) {
					return cl.Environments.TakeoverDesktop(ctx, in.ID)
				}
				if err := runDesktop(cmd.Context(), in.EnvID, open, listen, false, noOpen); err != nil {
					return err
				}
				infof("viewer closed; %s is still held. release it with: fuse hitl release %s --note \"...\"", in.EnvID, in.EnvID)
				return nil
			}
			if !isInteractive() {
				return errors.New("takeover needs an interactive terminal for its shell; pass --no-shell to only take the lock")
			}
			infof("holding %s (takeover %s, until %s). the agent is paused.", in.EnvID, in.ID, shortTime(in.Deadline))
			rows, cols := terminalSize()
			stream, err := cl.Environments.TakeoverAttach(cmd.Context(), in.ID, fuse.AttachOptions{Rows: rows, Cols: cols})
			if err != nil {
				return friendly(err)
			}
			if err := pumpShell(stream); err != nil {
				return err
			}
			infof("shell closed; %s is still held. release it with: fuse hitl release %s --note \"...\"", in.EnvID, in.EnvID)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, recorded when no agent request is pending")
	cmd.Flags().BoolVar(&noShell, "no-shell", false, "take the lock without opening a shell or viewer")
	cmd.Flags().StringVar(&via, "via", fuse.TakeoverViaTerminal, "terminal (your own shell) or desktop (the live desktop view)")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:0", "with --via desktop: where the local viewer listens")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "with --via desktop: print the viewer url instead of opening a browser")
	addActorFlag(cmd, &actor)
	return cmd
}

func newHitlReleaseCmd() *cobra.Command {
	var actor, note string
	cmd := &cobra.Command{
		Use:   "release <env>",
		Short: "End a takeover and hand control back to the agent",
		Long: "ends the takeover held on an environment. the note is handed back to the\n" +
			"agent as the takeover's answer, so say what you changed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := hitlClient(actor)
			if err != nil {
				return err
			}
			in, err := cl.Environments.Release(cmd.Context(), args[0], note)
			if err != nil {
				return friendly(err)
			}
			if app.isJSON() {
				return printJSON(in)
			}
			successf("released %s (takeover %s)", in.EnvID, in.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "what you did, handed back to the agent")
	addActorFlag(cmd, &actor)
	return cmd
}
