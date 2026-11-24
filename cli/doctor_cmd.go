package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor <id>",
		Short: "Run an environment's in-guest self-check",
		Long: "Ask an environment's guest agent to check the pieces the sandbox depends\n" +
			"on and print one line per check: identity, dbus, disk and memory, each\n" +
			"display piece (desktop images), the healthcheck, dns, egress and the clock.\n\n" +
			"    fuse doctor vm-1\n" +
			"    fuse doctor vm-1 -o json\n\n" +
			"Each check is PASS, WARN, FAIL or SKIP (it does not apply here), and every\n" +
			"check runs whatever the others found. The command exits 1 when any check\n" +
			"fails, so it can gate a script.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, _, err := app.client()
			if err != nil {
				return err
			}
			rep, err := cl.Environments.Doctor(cmd.Context(), args[0])
			if err != nil {
				return friendly(err)
			}
			if rep.Status == fuse.DoctorFail {
				// a failing check is the answer, not a cli error: the
				// report is printed in full and the status rides out on
				// the exit code, as with a failed guest command in exec.
				app.exitCode = 1
			}
			if app.isJSON() {
				return printJSON(rep)
			}
			renderDoctor(rep)
			return nil
		},
	}
}

// renderDoctor prints one `PASS name: detail` line per check, the same lines
// fused writes to its journal at boot, then the overall verdict.
func renderDoctor(rep *fuse.DoctorReport) {
	for _, c := range rep.Checks {
		line := doctorStyle(c.Status) + " " + c.Name
		if c.Message != "" {
			line += ": " + c.Message
		}
		fmt.Println(line)
	}
	fmt.Println()
	fmt.Println(doctorStyle(rep.Status) + " " + styleFaint.Render("fused "+dash(rep.Version)))
}

// doctorStyle renders a check outcome as its padded uppercase label, colored.
func doctorStyle(status string) string {
	label := fmt.Sprintf("%-4s", strings.ToUpper(status))
	switch status {
	case fuse.DoctorPass:
		return styleGood.Render(label)
	case fuse.DoctorWarn:
		return styleWarn.Render(label)
	case fuse.DoctorFail:
		return styleBad.Render(label)
	case fuse.DoctorSkip:
		return styleFaint.Render(label)
	default:
		return label
	}
}
