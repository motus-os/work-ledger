package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/motus-os/work-ledger/internal/cli"
	"github.com/motus-os/work-ledger/internal/signals"
)

func main() {
	signals.IgnoreBrokenPipe()
	ctx, stop := signal.NotifyContext(context.Background(), signals.Process()...)
	defer stop()
	workingDirectory, err := os.Getwd()
	if err != nil {
		workingDirectory = "."
	}
	code := cli.Run(ctx, os.Args[1:], cli.Environment{
		WorkingDirectory: workingDirectory,
		Getenv:           os.Getenv,
		ProgramName:      os.Args[0],
		Stdin:            os.Stdin,
		Stdout:           os.Stdout,
		Stderr:           os.Stderr,
	})
	os.Exit(code)
}
