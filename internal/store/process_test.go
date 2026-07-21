package store

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestStoreHelperProcess(t *testing.T) {
	if os.Getenv("MOTUS_STORE_HELPER") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+3 >= len(os.Args) {
		os.Exit(98)
	}
	mode := os.Args[separator+1]
	stateDir := os.Args[separator+2]
	worker := os.Args[separator+3]
	ledger, err := Open(context.Background(), stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(97)
	}
	if mode == "crash" {
		runID := "run_crash_" + worker
		_, err = ledger.StartRun(context.Background(), StartRunParams{
			ID: runID, EventID: "event_" + runID + "_started", StartedAt: time.Now().UTC(), Source: "process-test", Producer: "test",
			ExecutableBasename: "helper",
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(96)
		}
		marker := os.Args[separator+4]
		if err := os.WriteFile(marker, []byte("ready"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(95)
		}
		for {
			time.Sleep(time.Hour)
		}
	}

	count, err := strconv.Atoi(os.Args[separator+4])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(94)
	}
	for index := range count {
		runID := fmt.Sprintf("run_process_%s_%03d", worker, index)
		_, err := ledger.StartRun(context.Background(), StartRunParams{
			ID: runID, EventID: "event_" + runID + "_started", StartedAt: time.Now().UTC(), Source: "process-test", Producer: "test",
			ExecutableBasename: "helper",
		})
		if err == nil {
			exitCode := 0
			_, err = ledger.CloseRun(context.Background(), runID, CloseRunParams{
				EventID: "terminal_" + runID, EndedAt: time.Now().UTC(), Outcome: OutcomeSuccess, ExitCode: &exitCode,
			})
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(93)
		}
	}
	if err := ledger.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(92)
	}
	os.Exit(0)
}

func storeHelperCommand(t *testing.T, arguments ...string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	commandArguments := []string{"-test.run=TestStoreHelperProcess", "--"}
	commandArguments = append(commandArguments, arguments...)
	command := exec.Command(executable, commandArguments...)
	command.Env = append(os.Environ(), "MOTUS_STORE_HELPER=1")
	return command
}

func TestMultipleProcessesWriteWithoutGaps(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	const workers = 4
	const runsPerWorker = 6
	commands := make([]*exec.Cmd, 0, workers)
	outputs := make([]bytes.Buffer, workers)
	for worker := range workers {
		command := storeHelperCommand(t, "write", stateDir, strconv.Itoa(worker), strconv.Itoa(runsPerWorker))
		command.Stdout = &outputs[worker]
		command.Stderr = &outputs[worker]
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
	}
	for index, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("worker %d: %v: %s", index, err, outputs[index].Bytes())
		}
	}
	ledger, err := OpenReadOnly(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	runs, err := ledger.ListRuns(context.Background(), ListRunsOptions{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(runs), workers*runsPerWorker; got != want {
		t.Fatalf("run count = %d, want %d", got, want)
	}
	for _, run := range runs {
		if run.State != RunClosed || run.Outcome != OutcomeSuccess {
			t.Fatalf("run = %#v", run)
		}
	}
	report, err := ledger.Doctor(context.Background())
	if err != nil || !report.Consistent {
		t.Fatalf("Doctor() = %#v, %v", report, err)
	}
}

func TestAbruptWriterTerminationLeavesConsistentStore(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	marker := filepath.Join(root, "ready")
	command := storeHelperCommand(t, "crash", stateDir, "one", marker)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatal("helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()

	ledger, err := Open(context.Background(), stateDir)
	if err != nil {
		t.Fatalf("Open after crash: %v", err)
	}
	defer ledger.Close()
	report, err := ledger.Doctor(context.Background())
	if err != nil || !report.Consistent {
		t.Fatalf("Doctor after crash = %#v, %v", report, err)
	}
	run, err := getRun(context.Background(), ledger.db, "run_crash_one")
	if err != nil || run.State != RunOpen {
		t.Fatalf("crash run = %#v, %v", run, err)
	}
}
