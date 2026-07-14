package main

import (
	"context"
	"os"

	"github.com/charmbracelet/fang"
)

func main() {
	if err := fang.Execute(context.Background(), newRootCmd(), fang.WithVersion(version)); err != nil {
		os.Exit(1)
	}
	// A guest command that exited non-zero ran fine; we just report its status.
	if app.exitCode != 0 {
		os.Exit(app.exitCode)
	}
}
