package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

func newOperationCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "operation",
		Aliases: []string{"operations", "op"},
		Short:   "Show or wait on the operation a mutating call ran as",
		Long: "create, destroy, fork, migrate, snapshot, restore and drain run as operations.\n" +
			"one that outlasts the server's attach bound is handed back as an operation id,\n" +
			"which the cli prints and then follows. interrupting the cli leaves the\n" +
			"operation running; pick it up again with `fuse operation wait <id>`.",
	}
	cmd.AddCommand(newOperationShowCmd(), newOperationWaitCmd())
	return cmd
}

func newOperationShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show an operation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			op, err := cl.Operations.Get(cmd.Context(), args[0], fuse.GetOperationOptions{})
			if err != nil {
				return friendly(err)
			}
			return renderOperation(op)
		},
	}
}

func newOperationWaitCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait <id>",
		Short: "Wait for an operation to finish and print its result",
		Long: "waits until the operation finishes and prints it. exits non-zero when the\n" +
			"operation failed, or when --timeout passes first (the operation keeps running).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			op, err := cl.Operations.Wait(ctx, args[0])
			if err != nil {
				if ctx.Err() != nil && cmd.Context().Err() == nil {
					return fmt.Errorf("operation %s still running after %s", args[0], timeout)
				}
				return friendly(err)
			}
			if err := renderOperation(op); err != nil {
				return err
			}
			if op.State == fuse.OperationFailed {
				return operationError(op)
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up waiting after this long (0 waits until it finishes)")
	return cmd
}

// operationError is a failed operation as a cli error.
func operationError(op *fuse.Operation) error {
	if op.Error == nil {
		return fmt.Errorf("operation %s failed with http %d", op.ID, op.StatusCode)
	}
	return friendly(&fuse.APIError{
		Status:  op.StatusCode,
		Code:    op.Error.Code,
		Message: op.Error.Message,
		Details: op.Error.Details,
	})
}

func renderOperation(op *fuse.Operation) error {
	if app.isJSON() {
		return printJSON(op)
	}
	finished := ""
	if op.FinishedAt != nil {
		finished = shortTime(*op.FinishedAt)
	}
	status := ""
	if op.StatusCode != 0 {
		status = strconv.Itoa(op.StatusCode) + " " + http.StatusText(op.StatusCode)
	}
	pairs := [][2]string{
		{"id", op.ID},
		{"kind", op.Kind},
		{"state", stateStyle(op.State)},
		{"step", dash(op.Step)},
		{"started", shortTime(op.StartedAt)},
		{"finished", dash(finished)},
		{"status", dash(status)},
	}
	if op.Error != nil {
		pairs = append(pairs, [2]string{"error", op.Error.Code + ": " + op.Error.Message})
	}
	renderDetail(pairs)
	if len(op.Result) > 0 {
		fmt.Println()
		fmt.Println(string(op.Result))
	}
	return nil
}

// printOperation tells the user a call outlasted the server's attach bound,
// before the sdk follows it.
func printOperation(acc *fuse.OperationAccepted) {
	fmt.Fprintf(os.Stderr, "%s is still running as operation %s; waiting for it\n", acc.Operation.Kind, acc.OperationID)
	fmt.Fprintf(os.Stderr, "  interrupting leaves it running: fuse operation wait %s\n", acc.OperationID)
}
