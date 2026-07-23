// Package cli implements the Motus command-line interface.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/motus-os/work-ledger/internal/buildinfo"
	"github.com/motus-os/work-ledger/internal/capture"
	"github.com/motus-os/work-ledger/internal/gitmeta"
	"github.com/motus-os/work-ledger/internal/ids"
	"github.com/motus-os/work-ledger/internal/signals"
	"github.com/motus-os/work-ledger/internal/statepath"
	"github.com/motus-os/work-ledger/internal/store"
)

const internalFailureExitCode = 125

// Environment supplies process-level dependencies without making tests change
// global working-directory or environment state.
type Environment struct {
	WorkingDirectory string
	Getenv           func(string) string
	ProgramName      string
	Stdin            io.Reader
	Stdout           io.Writer
	Stderr           io.Writer
}

// Run executes the CLI and returns the process exit status.
func Run(ctx context.Context, arguments []string, environment Environment) (code int) {
	defer func() {
		code = processSignalExitCode(ctx, code)
	}()
	environment = normalizeEnvironment(environment)
	options, remaining, err := parseRootOptions(arguments)
	if err != nil {
		return usageError(environment.Stderr, err)
	}
	if options.help {
		writeRootHelp(environment.Stdout)
		return 0
	}
	if options.version {
		if len(remaining) != 0 {
			return usageError(environment.Stderr, errors.New("--version takes no arguments"))
		}
		writeVersion(environment.Stdout)
		return 0
	}
	if len(remaining) == 0 {
		writeRootHelp(environment.Stdout)
		return 0
	}
	environmentStateDir := environment.Getenv(statepath.EnvironmentVariable)
	stateDir, err := statepath.Resolve(
		ctx,
		environment.WorkingDirectory,
		options.stateDir,
		environmentStateDir,
	)
	if err != nil {
		return commandError(environment.Stderr, err)
	}

	switch remaining[0] {
	case "wrap":
		return runWrap(ctx, remaining[1:], stateDir, environment)
	case "run":
		return runRun(ctx, remaining[1:], stateDir, environment)
	case "finding":
		return runFinding(ctx, remaining[1:], stateDir, environment)
	case "doctor":
		return runDoctor(ctx, remaining[1:], stateDir, environment)
	case "version":
		if len(remaining) != 1 {
			return usageError(environment.Stderr, errors.New("version takes no arguments"))
		}
		writeVersion(environment.Stdout)
		return 0
	case "help":
		writeRootHelp(environment.Stdout)
		return 0
	default:
		return usageError(environment.Stderr, fmt.Errorf("unknown command %q", remaining[0]))
	}
}

func processSignalExitCode(ctx context.Context, code int) int {
	number := signals.CancellationNumber(ctx)
	if number > 0 && number <= 127 {
		return 128 + number
	}
	return code
}

type rootOptions struct {
	stateDir string
	help     bool
	version  bool
}

func parseRootOptions(arguments []string) (rootOptions, []string, error) {
	var options rootOptions
	for len(arguments) > 0 {
		switch arguments[0] {
		case "--state-dir":
			if len(arguments) < 2 || arguments[1] == "" {
				return rootOptions{}, nil, errors.New("--state-dir requires a path")
			}
			options.stateDir = arguments[1]
			arguments = arguments[2:]
		case "-h", "--help":
			options.help = true
			arguments = arguments[1:]
		case "-V", "--version":
			options.version = true
			arguments = arguments[1:]
		default:
			return options, arguments, nil
		}
	}
	return options, nil, nil
}

func normalizeEnvironment(environment Environment) Environment {
	if environment.WorkingDirectory == "" {
		environment.WorkingDirectory, _ = os.Getwd()
	}
	if environment.Getenv == nil {
		environment.Getenv = os.Getenv
	}
	if environment.ProgramName == "" {
		environment.ProgramName = "motus"
	}
	if environment.Stdin == nil {
		environment.Stdin = strings.NewReader("")
	}
	if environment.Stdout == nil {
		environment.Stdout = io.Discard
	}
	if environment.Stderr == nil {
		environment.Stderr = io.Discard
	}
	return environment
}

func runWrap(ctx context.Context, arguments []string, stateDir string, environment Environment) int {
	if len(arguments) == 1 && (arguments[0] == "-h" || arguments[0] == "--help") {
		writeWrapHelp(environment.Stdout)
		return 0
	}
	if len(arguments) < 2 || arguments[0] != "--" || arguments[1] == "" {
		return usageError(environment.Stderr, errors.New("wrap requires -- followed by a command"))
	}
	command := arguments[1]
	commandArguments := arguments[2:]

	runID, err := ids.New("run_")
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	startEventID, err := ids.New("event_")
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	startedAt := time.Now().UTC().Round(0)
	// Read Git state before creating .motus so the ledger's own first file does
	// not make an otherwise clean repository appear dirty.
	git := gitmeta.Detect(ctx, environment.WorkingDirectory)
	dirty := git.Dirty
	gitRecord := store.GitMetadata{}
	if git.Root != "" {
		gitRecord.Repository = filepath.Base(git.Root)
		gitRecord.Commit = git.HeadSHA
		gitRecord.Dirty = &dirty
	}
	ledger, err := store.Open(ctx, stateDir)
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	defer ledger.Close()
	_, err = ledger.StartRun(ctx, store.StartRunParams{
		ID:                 runID,
		EventID:            startEventID,
		StartedAt:          startedAt,
		Source:             "motus-wrap",
		Producer:           "motus",
		ExecutableBasename: filepath.Base(command),
		ArgumentCount:      len(commandArguments),
		Git:                gitRecord,
	})
	if err != nil {
		return commandError(environment.Stderr, err)
	}

	childStderr := &lastByteWriter{destination: environment.Stderr}
	result := capture.Run(ctx, command, commandArguments, environment.Stdin, environment.Stdout, childStderr)
	endedAt := time.Now().UTC().Round(0)
	terminalEventID, err := ids.New("event_")
	if err != nil {
		return commandError(environment.Stderr, fmt.Errorf("run %s remains open: %w", runID, err))
	}
	stdoutBytes, stdoutLines, err := boundedOutputCounts(result.Stdout)
	if err != nil {
		return commandError(environment.Stderr, fmt.Errorf("run %s remains open: %w", runID, err))
	}
	stderrBytes, stderrLines, err := boundedOutputCounts(result.Stderr)
	if err != nil {
		return commandError(environment.Stderr, fmt.Errorf("run %s remains open: %w", runID, err))
	}
	exitCode := optionalExitCode(result.ExitCode)
	timedOut := result.Failures.Has(capture.FailureTimeout)
	var signalName *string
	if result.Signal != "" {
		signalName = &result.Signal
	}
	outcome := captureOutcome(result)
	_, err = ledger.CloseRun(context.WithoutCancel(ctx), runID, store.CloseRunParams{
		EventID:  terminalEventID,
		EndedAt:  endedAt,
		Outcome:  outcome,
		ExitCode: exitCode,
		Signal:   signalName,
		TimedOut: &timedOut,
		Output: store.OutputCounts{
			StdoutBytes: stdoutBytes,
			StderrBytes: stderrBytes,
			StdoutLines: stdoutLines,
			StderrLines: stderrLines,
		},
	})
	if err != nil {
		return commandError(environment.Stderr, fmt.Errorf("command finished but run %s could not be closed: %w", runID, err))
	}
	if childStderr.wrote && childStderr.last != '\n' {
		fmt.Fprintln(environment.Stderr)
	}
	fmt.Fprintf(environment.Stderr, "motus: recorded %s (%s)\n", runID, outcome)
	fmt.Fprintf(environment.Stderr, "%s %s\n", nextCommandLabel(), displayCommand(
		environment.ProgramName,
		"--state-dir", stateDir,
		"run", "receipt", runID,
	))
	return captureExitCode(result)
}

func displayCommand(arguments ...string) string {
	return displayCommandForPlatform(runtime.GOOS, arguments...)
}

func displayCommandForPlatform(goos string, arguments ...string) string {
	formatted := make([]string, 0, len(arguments))
	if goos == "windows" {
		for _, argument := range arguments {
			formatted = append(formatted, "'"+strings.ReplaceAll(argument, "'", "''")+"'")
		}
		return "& " + strings.Join(formatted, " ")
	}
	for _, argument := range arguments {
		if argument != "" && strings.IndexFunc(argument, func(r rune) bool {
			return !(r == '_' || r == '-' || r == '.' || r == '/' || r == '\\' || r == ':' ||
				r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z')
		}) == -1 {
			formatted = append(formatted, argument)
		} else {
			formatted = append(formatted, "'"+strings.ReplaceAll(argument, "'", `'"'"'`)+"'")
		}
	}
	return strings.Join(formatted, " ")
}

func nextCommandLabel() string {
	if runtime.GOOS == "windows" {
		return "Next (PowerShell):"
	}
	return "Next:"
}

type lastByteWriter struct {
	destination io.Writer
	wrote       bool
	last        byte
}

func (w *lastByteWriter) Write(payload []byte) (int, error) {
	n, err := w.destination.Write(payload)
	if n > 0 {
		w.wrote = true
		w.last = payload[n-1]
	}
	return n, err
}

func boundedOutputCounts(stats capture.StreamStats) (int64, int64, error) {
	if stats.Bytes > math.MaxInt64 || stats.Lines > math.MaxInt64 {
		return 0, 0, errors.New("captured output counts exceed ledger limits")
	}
	return int64(stats.Bytes), int64(stats.Lines), nil
}

func optionalExitCode(exitCode int) *int {
	if exitCode < 0 {
		return nil
	}
	value := exitCode
	return &value
}

func captureOutcome(result capture.Result) store.Outcome {
	if result.Failures.Has(capture.FailureOutputCopy) {
		return store.OutcomeFailure
	}
	if result.Failures.Has(capture.FailureTimeout) || result.Failures.Has(capture.FailureCanceled) {
		return store.OutcomeAborted
	}
	if result.Failures != 0 || result.ExitCode != 0 {
		return store.OutcomeFailure
	}
	return store.OutcomeSuccess
}

func captureExitCode(result capture.Result) int {
	if result.Failures.Has(capture.FailureOutputCopy) {
		return internalFailureExitCode
	}
	if result.Failures.Has(capture.FailureTimeout) {
		return 124
	}
	if result.Failures.Has(capture.FailureCanceled) {
		if result.CancellationSignalNumber > 0 && result.CancellationSignalNumber <= 127 {
			return 128 + result.CancellationSignalNumber
		}
		return 130
	}
	if result.Failures.Has(capture.FailureStart) {
		return 127
	}
	if result.Failures.Has(capture.FailureSignal) && result.SignalNumber > 0 && result.SignalNumber <= 127 {
		return 128 + result.SignalNumber
	}
	if result.ExitCode >= 0 && result.ExitCode <= 255 {
		return result.ExitCode
	}
	return 1
}

func runRun(ctx context.Context, arguments []string, stateDir string, environment Environment) int {
	if len(arguments) == 0 || arguments[0] == "-h" || arguments[0] == "--help" {
		writeRunHelp(environment.Stdout)
		return 0
	}
	switch arguments[0] {
	case "list":
		return runList(ctx, arguments[1:], stateDir, environment)
	case "receipt":
		return runReceipt(ctx, arguments[1:], stateDir, environment)
	default:
		return usageError(environment.Stderr, fmt.Errorf("unknown run command %q", arguments[0]))
	}
}

func runList(ctx context.Context, arguments []string, stateDir string, environment Environment) int {
	jsonOutput := false
	options := store.ListRunsOptions{}
	for len(arguments) > 0 {
		argument := arguments[0]
		switch argument {
		case "--json":
			jsonOutput = true
			arguments = arguments[1:]
		case "--limit", "--offset", "--state", "--outcome":
			if len(arguments) < 2 {
				return usageError(environment.Stderr, fmt.Errorf("%s requires a value", argument))
			}
			value := arguments[1]
			arguments = arguments[2:]
			switch argument {
			case "--limit":
				parsed, err := strconv.Atoi(value)
				if err != nil {
					return usageError(environment.Stderr, fmt.Errorf("invalid --limit %q", value))
				}
				options.Limit = parsed
			case "--offset":
				parsed, err := strconv.Atoi(value)
				if err != nil {
					return usageError(environment.Stderr, fmt.Errorf("invalid --offset %q", value))
				}
				options.Offset = parsed
			case "--state":
				options.State = store.RunState(value)
			case "--outcome":
				options.Outcome = store.Outcome(value)
			}
		case "-h", "--help":
			writeRunHelp(environment.Stdout)
			return 0
		default:
			return usageError(environment.Stderr, fmt.Errorf("unknown run list option %q", argument))
		}
	}
	ledger, err := store.OpenForRead(ctx, stateDir)
	if errors.Is(err, store.ErrNotFound) {
		if jsonOutput {
			fmt.Fprintln(environment.Stdout, "[]")
		} else {
			fmt.Fprintln(environment.Stdout, "No runs recorded.")
		}
		return 0
	}
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	defer ledger.Close()
	runs, err := ledger.ListRuns(ctx, options)
	if errors.Is(err, store.ErrInvalid) {
		return usageError(environment.Stderr, err)
	}
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	if jsonOutput {
		return writeJSON(environment.Stdout, runs, environment.Stderr)
	}
	if len(runs) == 0 {
		fmt.Fprintln(environment.Stdout, "No runs recorded.")
		return 0
	}
	writer := tabwriter.NewWriter(environment.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "RUN ID\tSTATE\tOUTCOME\tSTARTED (UTC)\tCOMMAND")
	for _, run := range runs {
		outcome := string(run.Outcome)
		if outcome == "" {
			outcome = "-"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			safeHumanText(run.ID), run.State, outcome, run.StartedAt.UTC().Format(time.RFC3339), safeHumanText(run.ExecutableBasename))
	}
	if err := writer.Flush(); err != nil {
		return commandError(environment.Stderr, fmt.Errorf("write run list: %w", err))
	}
	return 0
}

func safeHumanText(value string) string {
	if strings.IndexFunc(value, unicode.IsControl) == -1 {
		return value
	}
	return strconv.QuoteToGraphic(value)
}

func runReceipt(ctx context.Context, arguments []string, stateDir string, environment Environment) int {
	if len(arguments) != 1 || arguments[0] == "-h" || arguments[0] == "--help" {
		if len(arguments) == 1 && (arguments[0] == "-h" || arguments[0] == "--help") {
			writeRunHelp(environment.Stdout)
			return 0
		}
		return usageError(environment.Stderr, errors.New("run receipt requires one run ID"))
	}
	ledger, err := store.OpenForRead(ctx, stateDir)
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	defer ledger.Close()
	receipt, err := ledger.ProjectReceipt(ctx, arguments[0])
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	if _, err := environment.Stdout.Write(receipt.Bytes()); err != nil {
		return commandError(environment.Stderr, fmt.Errorf("write receipt: %w", err))
	}
	return 0
}

func runDoctor(ctx context.Context, arguments []string, stateDir string, environment Environment) int {
	jsonOutput := false
	for _, argument := range arguments {
		switch argument {
		case "--json":
			jsonOutput = true
		case "-h", "--help":
			writeDoctorHelp(environment.Stdout)
			return 0
		default:
			return usageError(environment.Stderr, fmt.Errorf("unknown doctor option %q", argument))
		}
	}
	ledger, err := store.OpenForRead(ctx, stateDir)
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	defer ledger.Close()
	report, err := ledger.Doctor(ctx)
	if err != nil {
		return commandError(environment.Stderr, err)
	}
	if jsonOutput {
		if code := writeJSON(environment.Stdout, report, environment.Stderr); code != 0 {
			return code
		}
	} else {
		for _, check := range report.Checks {
			status := "PASS"
			if !check.OK {
				status = "FAIL"
			}
			fmt.Fprintf(environment.Stdout, "%s  %s: %s\n", status, check.Name, check.Detail)
		}
		fmt.Fprintf(environment.Stdout, "\nScope: %s.\n", report.Scope)
	}
	if !report.Consistent {
		return 1
	}
	return 0
}

func writeJSON(destination io.Writer, value any, stderr io.Writer) int {
	encoder := json.NewEncoder(destination)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return commandError(stderr, fmt.Errorf("write JSON: %w", err))
	}
	return 0
}

func writeText(destination io.Writer, text string, stderr io.Writer) int {
	if _, err := io.WriteString(destination, text); err != nil {
		return commandError(stderr, fmt.Errorf("write output: %w", err))
	}
	return 0
}

func writeVersion(destination io.Writer) {
	parts := []string{"motus", buildinfo.EffectiveVersion()}
	if buildinfo.Commit != "" && buildinfo.Commit != "unknown" {
		parts = append(parts, "commit "+buildinfo.Commit)
	}
	if buildinfo.Date != "" && buildinfo.Date != "unknown" {
		parts = append(parts, "built "+buildinfo.Date)
	}
	fmt.Fprintln(destination, strings.Join(parts, " "))
}

func usageError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "motus: %v\nTry 'motus --help'.\n", err)
	return 2
}

func commandError(stderr io.Writer, err error) int {
	if errors.Is(err, context.Canceled) {
		return 1
	}
	fmt.Fprintf(stderr, "motus: %v\n", err)
	return 1
}

func writeRootHelp(destination io.Writer) {
	commands := []string{
		"wrap -- COMMAND [ARG ...]  Run a command and record selected metadata",
		"run list [--json]          List recorded runs",
		"run receipt RUN_ID         Write a JSON receipt for a closed run",
		"finding add [OPTIONS]       Connect an authored finding to a run",
		"finding list [OPTIONS]      List and search findings",
		"finding show FINDING_ID     Show a finding and its run context",
		"finding close [OPTIONS]     Resolve or dismiss a finding",
		"doctor [--json]            Check local database consistency",
		"version                    Print version information",
	}
	fmt.Fprintln(destination, `Motus Work Ledger

Usage:
  motus [--state-dir PATH] COMMAND

Commands:`)
	for _, command := range commands {
		fmt.Fprintf(destination, "  %s\n", command)
	}
	fmt.Fprintln(destination, `
State:
  --state-dir PATH          Override the .motus state directory
  MOTUS_STATE_DIR           Environment alternative to --state-dir

Motus records producer-controlled process metadata. It does not capture raw
command output or establish that recorded work was correct.`)
}

func writeWrapHelp(destination io.Writer) {
	fmt.Fprintln(destination, `Usage: motus [--state-dir PATH] wrap -- COMMAND [ARG ...]

Runs COMMAND directly without a shell. Motus forwards stdin, copies stdout and
stderr to the original destinations, and stores metadata, output counts, and
outcome. Programs can detect that their output is being copied through pipes.`)
}

func writeRunHelp(destination io.Writer) {
	fmt.Fprintln(destination, `Usage:
  motus [--state-dir PATH] run list [--json] [--limit N] [--offset N]
      [--state open|closed] [--outcome success|failure|aborted]
  motus [--state-dir PATH] run receipt RUN_ID`)
}

func writeDoctorHelp(destination io.Writer) {
	fmt.Fprintln(destination, `Usage: motus [--state-dir PATH] doctor [--json]

Checks current SQLite structure and internal consistency. A passing result is
not independent authentication of producer-controlled records.`)
}
