//go:build unix

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/motus-os/work-ledger/internal/cli"
	"github.com/motus-os/work-ledger/internal/signals"
	"github.com/motus-os/work-ledger/internal/store"
)

const brokenPipeOutputBytes = 1 << 20

func TestBrokenPipeLifecycleProcess(t *testing.T) {
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}
	mode := os.Args[separator+1]
	switch mode {
	case "writer":
		if _, err := io.CopyN(os.Stdout, strings.NewReader(strings.Repeat("x", brokenPipeOutputBytes)), brokenPipeOutputBytes); err != nil {
			os.Exit(91)
		}
		os.Exit(0)
	case "runner":
		if separator+2 >= len(os.Args) {
			os.Exit(92)
		}
		signals.IgnoreBrokenPipe()
		cwd, err := os.Getwd()
		if err != nil {
			os.Exit(93)
		}
		stateDir := os.Args[separator+2]
		child := []string{
			"--state-dir", stateDir,
			"wrap", "--", os.Args[0],
			"-test.run=^TestBrokenPipeLifecycleProcess$", "--", "writer",
		}
		code := cli.Run(context.Background(), child, cli.Environment{
			WorkingDirectory: cwd,
			Getenv:           os.Getenv,
			ProgramName:      os.Args[0],
			Stdin:            strings.NewReader(""),
			Stdout:           os.Stdout,
			Stderr:           os.Stderr,
		})
		os.Exit(code)
	}
}

func TestBrokenOutputPipeStillClosesTheRun(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		os.Args[0],
		"-test.run=^TestBrokenPipeLifecycleProcess$",
		"--", "runner", stateDir,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	var exitErr *exec.ExitError
	if !errorsAs(err, &exitErr) || exitErr.ExitCode() != 125 {
		t.Fatalf("runner error = %v, stderr=%q", err, stderr.String())
	}
	if ctx.Err() != nil {
		t.Fatalf("runner timed out: %v", ctx.Err())
	}

	ledger, err := store.OpenReadOnly(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	runs, err := ledger.ListRuns(context.Background(), store.ListRunsOptions{})
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns() = %#v, %v", runs, err)
	}
	run := runs[0]
	if run.State != store.RunClosed || run.Outcome != store.OutcomeFailure ||
		run.Output.StdoutBytes != brokenPipeOutputBytes {
		t.Fatalf("broken-pipe run = %#v", run)
	}
}

func errorsAs(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError)
	if ok {
		*target = exitErr
	}
	return ok
}
