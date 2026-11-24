package main

import (
	"github.com/spf13/cobra"

	fuse "github.com/folsomintel/fuse/sdks/go"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor <id>",
		Short: "Run an environment's in-guest self-check",
		Long: "Ask an environment's guest agent to check the pieces the sandbox depends\n" +
			"on and print one line per check: the agent itself, the display, window\n" +
			"manager and vnc server (desktop images), the healthcheck, disk and memory.\n\n" +
			"    fuse doctor vm-1\n" +
			"    fuse doctor vm-1 -o json\n\n" +
			"Each check is ok, warn, fail or skip (it does not apply here). The command\n" +
			"exits 1 when any check fails, so it can gate a script.",
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

func renderDoctor(rep *fuse.DoctorReport) {
	rows := make([][]string, 0, len(rep.Checks))
	for _, c := range rep.Checks {
		rows = append(rows, []string{c.Name, doctorStyle(c.Status), dash(c.Message)})
	}
	renderTable([]string{"check", "status", "detail"}, rows)
	infof("%s  %s", doctorStyle(rep.Status), styleFaint.Render("fused "+dash(rep.Version)))
}

// doctorStyle colors a check outcome.
func doctorStyle(status string) string {
	switch status {
	case fuse.DoctorOK:
		return styleGood.Render(status)
	case fuse.DoctorWarn:
		return styleWarn.Render(status)
	case fuse.DoctorFail:
		return styleBad.Render(status)
	case fuse.DoctorSkip:
		return styleFaint.Render(status)
	default:
		return status
	}
}
