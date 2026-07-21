//go:build unix

package capture

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	unixHelperMarker            = "capture-unix-test-helper"
	unixDescendantReady         = "unix-descendant-ready\n"
	unixParentReady             = "unix-parent-ready\n"
	unixDescendantSurvivalDelay = time.Second
)

// TestCaptureUnixHelperProcess builds a two-generation process tree. The
// descendant inherits the parent's process group and output descriptors. If
// Run kills only the direct child, output forwarding remains open until the
// descendant writes the survival marker and exits.
func TestCaptureUnixHelperProcess(t *testing.T) {
	args, ok := testHelperInvocation(unixHelperMarker)
	if !ok {
		return
	}
	os.Exit(runUnixCaptureHelper(args))
}

func runUnixCaptureHelper(args []string) int {
	if len(args) != 2 {
		return helperInternalFailure
	}
	mode, survivalMarker := args[0], args[1]
	switch mode {
	case "signal":
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			return helperInternalFailure
		}
		time.Sleep(time.Second)
		return helperInternalFailure
	case "parent":
		childArgs := []string{
			"-test.run=^TestCaptureUnixHelperProcess$",
			"--",
			unixHelperMarker,
			"descendant",
			survivalMarker,
		}
		descendant := exec.Command(os.Args[0], childArgs...)
		descendant.Stdout = os.Stdout
		descendant.Stderr = os.Stderr
		if err := descendant.Start(); err != nil {
			return helperInternalFailure
		}
		if writeHelperOutput(os.Stdout, []byte(unixParentReady)) != nil {
			_ = descendant.Process.Kill()
			_ = descendant.Wait()
			return helperInternalFailure
		}
		// This bounds a broken implementation while remaining much longer than
		// the descendant's survival-marker path.
		time.Sleep(6 * time.Second)
		_ = descendant.Process.Kill()
		_ = descendant.Wait()
		return 0
	case "descendant":
		if writeHelperOutput(os.Stdout, []byte(unixDescendantReady)) != nil {
			return helperInternalFailure
		}
		time.Sleep(unixDescendantSurvivalDelay)
		if err := os.WriteFile(survivalMarker, []byte("descendant survived cancellation\n"), 0o600); err != nil {
			return helperInternalFailure
		}
		time.Sleep(time.Second)
		return 0
	default:
		return helperInternalFailure
	}
}

func TestRunReportsExternalUnixSignal(t *testing.T) {
	args := []string{
		"-test.run=^TestCaptureUnixHelperProcess$",
		"--",
		unixHelperMarker,
		"signal",
		"unused",
	}
	result := Run(context.Background(), os.Args[0], args, nil, nil, nil)
	if !result.Failures.Has(FailureSignal) {
		t.Fatalf("Failures = %v, want FailureSignal", result.Failures)
	}
	if result.ExitCode != -1 || result.Signal != syscall.SIGTERM.String() || result.SignalNumber != int(syscall.SIGTERM) {
		t.Fatalf("signal result = %#v", result)
	}
}

func TestRunCancellationTerminatesUnixDescendants(t *testing.T) {
	survivalMarker := t.TempDir() + "/descendant-survived"
	ctx, cancel := context.WithCancel(context.Background())
	stdout := newSignalBuffer([]byte(unixDescendantReady))
	var stderr bytes.Buffer
	args := []string{
		"-test.run=^TestCaptureUnixHelperProcess$",
		"--",
		unixHelperMarker,
		"parent",
		survivalMarker,
	}
	resultc := make(chan Result, 1)
	defer cancel()

	go func() {
		resultc <- Run(ctx, os.Args[0], args, nil, stdout, &stderr)
	}()

	select {
	case <-stdout.Ready():
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Unix descendant did not become ready")
	}
	cancel()
	result := awaitResult(t, resultc, 8*time.Second)

	assertResultOutcome(t, result, -1, FailureCanceled)
	if _, err := os.Stat(survivalMarker); !os.IsNotExist(err) {
		if err == nil {
			t.Fatal("descendant survived process-group cancellation and wrote its marker")
		}
		t.Fatalf("stat descendant survival marker: %v", err)
	}
	stdoutBytes := stdout.Bytes()
	if !bytes.Contains(stdoutBytes, []byte(unixParentReady)) ||
		!bytes.Contains(stdoutBytes, []byte(unixDescendantReady)) {
		t.Fatalf("process-tree readiness output = %q", stdoutBytes)
	}
	assertStreamStats(t, "stdout", result.Stdout, stdoutBytes, false)
	assertStreamStats(t, "stderr", result.Stderr, stderr.Bytes(), false)
	if strings.Contains(fmt.Sprintf("%#v", result), survivalMarker) {
		t.Fatal("Result retained a descendant argument value")
	}
}
