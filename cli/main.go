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
}
