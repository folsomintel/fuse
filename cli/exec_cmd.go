package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

func newEnvExecCmd() *cobra.Command {
	var (
		shell   string
		timeout time.Duration
	)

	cmd := &cobra.Command{
		Use:   "exec <id> [-- <command> [args...]]",
		Short: "Run a command inside an environment's guest",
		Long: "Run a command inside a running environment's guest and print its output.\n\n" +
			"The command is passed as argv after `--`, which needs no quoting rules:\n\n" +
			"    fuse environment exec vm-1 -- ls -l /var/log\n\n" +
			"For a pipeline, redirect, or glob, use --shell, which runs the string\n" +
			"under `sh -lc`:\n\n" +
			"    fuse environment exec vm-1 --shell 'ls /var/log | wc -l'\n\n" +
			"fuse exits with the guest command's exit code, so it composes with && and\n" +
			"set -e. Exec requires the master token.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vmID := args[0]
			argv := args[1:]

			if shell != "" && len(argv) > 0 {
				return errors.New("pass a command or --shell, not both")
			}
			if shell == "" && len(argv) == 0 {
				return errors.New("nothing to run: pass a command after `--`, or use --shell")
			}

			cl, _, err := app.client()
			if err != nil {
				return err
			}

			res, err := cl.Environments.Exec(cmd.Context(), vmID, fuse.ExecRequest{
				Cmd:       argv,
				Shell:     shell,
				TimeoutMS: int(timeout.Milliseconds()),
			})
			if err != nil {
				return friendly(err)
			}

			// The guest's exit code becomes ours. A command that ran and failed is
			// not a CLI error, so this is reported by exiting, not by rendering.
			app.exitCode = res.ExitCode

			if app.isJSON() {
				return printJSON(res)
			}
			_, _ = io.WriteString(os.Stdout, res.Stdout)
			_, _ = io.WriteString(os.Stderr, res.Stderr)
			return nil
		},
	}

	cmd.Flags().StringVar(&shell, "shell", "",
		"run a shell one-liner via `sh -lc` (for pipelines, redirects, globs)")
	cmd.Flags().DurationVar(&timeout, "timeout", 0,
		"bound the command inside the guest (default: server default; server caps at 10m)")
	return cmd
}

func newEnvShellCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "shell <id> [-- <command> [args...]]",
		Aliases: []string{"ssh"},
		Short:   "Open an interactive shell inside an environment's guest",
		Long: "Attach an interactive terminal to a running environment's guest.\n\n" +
			"With no command this opens the guest's login shell. A command after `--`\n" +
			"runs instead of the shell, still on a terminal:\n\n" +
			"    fuse environment shell vm-1\n" +
			"    fuse environment shell vm-1 -- top\n\n" +
			"This needs a real terminal. For scripted commands use\n" +
			"`fuse environment exec`. Shell requires the master token.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShell(cmd.Context(), args[0], args[1:])
		},
	}
}

// runShell attaches the local terminal to a guest process.
//
// The guest's pty is what echoes keystrokes and handles line editing, so the
// local terminal must stop doing both: without raw mode you see every character
// twice and Ctrl-C kills the CLI instead of the guest command.
func runShell(ctx context.Context, vmID string, argv []string) error {
	if !isInteractive() {
		return errors.New("shell needs an interactive terminal; use `fuse environment exec` to run a command non-interactively")
	}

	cl, _, err := app.client()
	if err != nil {
		return err
	}

	rows, cols := terminalSize()
	stream, err := cl.Environments.Attach(ctx, vmID, fuse.AttachOptions{
		Cmd:  argv,
		Rows: rows,
		Cols: cols,
	})
	if err != nil {
		return friendly(err)
	}
	defer func() { _ = stream.Close() }()

	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("put terminal in raw mode: %w", err)
	}
	defer func() { _ = term.Restore(fd, state) }()

	stopResize := watchResize(stream)
	defer stopResize()

	// Local stdin becomes guest stdin. AttachStream frames each write, and
	// serializes them against the resize handler.
	go func() { _, _ = io.Copy(stream, os.Stdin) }()

	for {
		f, err := stream.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch f.Type {
		case fuse.FrameStdout, fuse.FrameStderr:
			// A pty merges the two, so there is nothing to separate here.
			_, _ = os.Stdout.Write(f.Payload)
		case fuse.FrameExit:
			var p fuse.ExitPayload
			if err := json.Unmarshal(f.Payload, &p); err == nil {
				app.exitCode = p.ExitCode
			}
			return nil
		}
	}
}

// watchResize forwards local terminal resizes to the guest for the life of the
// session, and returns a function that stops doing so.
func watchResize(stream *fuse.AttachStream) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)

	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				rows, cols := terminalSize()
				_ = stream.Resize(rows, cols)
			case <-done:
				return
			}
		}
	}()

	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// terminalSize reports the local terminal's size, falling back to a
// conventional 24x80 when it cannot be determined.
func terminalSize() (rows, cols uint16) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 || h <= 0 {
		return 24, 80
	}
	return uint16(h), uint16(w)
}
