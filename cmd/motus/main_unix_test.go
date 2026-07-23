//go:build unix

package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/motus-os/work-ledger/internal/cli"
	"github.com/motus-os/work-ledger/internal/signals"
	"github.com/motus-os/work-ledger/internal/store"
)

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
		payload := strings.Repeat("x", 32*1024)
		for {
			if _, err := io.WriteString(os.Stdout, payload); err != nil {
				os.Exit(91)
			}
		}
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
	case "signal-child":
		if _, err := io.WriteString(os.Stdout, "signal-child-ready\n"); err != nil {
			os.Exit(94)
		}
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "signal-runner":
		if separator+2 >= len(os.Args) {
			os.Exit(95)
		}
		signals.IgnoreBrokenPipe()
		ctx, stop := signals.NotifyContext(context.Background())
		cwd, err := os.Getwd()
		if err != nil {
			os.Exit(96)
		}
		stateDir := os.Args[separator+2]
		child := []string{
			"--state-dir", stateDir,
			"wrap", "--", os.Args[0],
			"-test.run=^TestBrokenPipeLifecycleProcess$", "--", "signal-child",
		}
		code := cli.Run(ctx, child, cli.Environment{
			WorkingDirectory: cwd,
			Getenv:           os.Getenv,
			ProgramName:      os.Args[0],
			Stdin:            strings.NewReader(""),
			Stdout:           os.Stdout,
			Stderr:           os.Stderr,
		})
		stop()
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
		run.Output.StdoutBytes == 0 ||
		run.Signal != nil {
		t.Fatalf("broken-pipe run = %#v", run)
	}
}

func TestProcessSignalsPreserveShellExitStatus(t *testing.T) {
	for _, test := range []struct {
		name     string
		signal   os.Signal
		exitCode int
	}{
		{name: "interrupt", signal: os.Interrupt, exitCode: 130},
		{name: "terminate", signal: syscall.SIGTERM, exitCode: 143},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(
				ctx,
				os.Args[0],
				"-test.run=^TestBrokenPipeLifecycleProcess$",
				"--", "signal-runner", stateDir,
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
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || line != "signal-child-ready\n" {
				_ = command.Process.Kill()
				_ = command.Wait()
				t.Fatalf("child readiness = %q, %v; stderr=%q", line, err, stderr.String())
			}
			if err := command.Process.Signal(test.signal); err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				t.Fatal(err)
			}
			err = command.Wait()
			var exitErr *exec.ExitError
			if !errorsAs(err, &exitErr) || exitErr.ExitCode() != test.exitCode {
				t.Fatalf("runner error = %v, want exit %d; stderr=%q", err, test.exitCode, stderr.String())
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
			if run.State != store.RunClosed || run.Outcome != store.OutcomeAborted ||
				run.Signal != nil || run.ExitCode != nil {
				t.Fatalf("canceled run = %#v", run)
			}
		})
	}
}

func errorsAs(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError)
	if ok {
		*target = exitErr
	}
	return ok
}
