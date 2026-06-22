package main

import (
	"os"

	"github.com/charmbracelet/huh"
	"github.com/mattn/go-isatty"
)

// isInteractive reports whether we can run an interactive huh form (both stdin
// and stdout are ttys).
func isInteractive() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}

// runForm runs a huh form built from the given groups.
func runForm(groups ...*huh.Group) error {
	return huh.NewForm(groups...).Run()
}

// confirm asks a yes/no question. it returns false without prompting when not
// interactive, so destructive commands must also support a --yes flag.
func confirm(title string) (bool, error) {
	if !isInteractive() {
		return false, nil
	}
	var ok bool
	if err := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title(title).Value(&ok))).Run(); err != nil {
		return false, err
	}
	return ok, nil
}
