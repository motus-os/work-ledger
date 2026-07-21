// Package capture runs a child process while forwarding and measuring its
// output without retaining the output or command arguments.
package capture

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
)

// Failure is a bit set describing failures in starting, supervising, or
// copying output from a command. More than one bit can be set; for example, a
// command can be terminated by a signal after its output writer has failed.
type Failure uint8

const (
	// FailureStart means the command did not start, or platform process
	// supervision could not be established before the command was allowed to run.
	FailureStart Failure = 1 << iota
	// FailureTimeout means the context deadline expired before the command and
	// all output forwarding completed.
	FailureTimeout
	// FailureSignal means the command was terminated by a signal not initiated
	// by this package in response to context cancellation.
	FailureSignal
	// FailureOutputCopy means at least one supplied output writer failed, or the
	// operating system reported an output-copy error while waiting.
	FailureOutputCopy
	// FailureCanceled means the context was canceled before the command and all
	// output forwarding completed.
	FailureCanceled
)

// Has reports whether f contains flag.
func (f Failure) Has(flag Failure) bool {
	return f&flag != 0
}

// CommandMetadata is the deliberately limited description of a command.
// Argument values and executable paths are never included.
type CommandMetadata struct {
	Executable    string
	ArgumentCount int
}

// StreamStats describes bytes observed from one child output stream. Lines is
// the number of newline delimiters observed; an unterminated final fragment is
// not counted as a newline-delimited line.
type StreamStats struct {
	Bytes      uint64
	Lines      uint64
	CopyFailed bool
}

// Result is the bounded result of a command run. ExitCode is -1 when no
// ordinary numeric exit code is available, including start and Unix signal
// failures. A non-zero ExitCode alone is an ordinary command outcome and does
// not set a Failure bit. Signal and SignalNumber are populated only when an
// external Unix signal terminates the command; cancellation signals used by
// this package are reported as cancellation or timeout instead.
type Result struct {
	Command      CommandMetadata
	Stdout       StreamStats
	Stderr       StreamStats
	ExitCode     int
	Signal       string
	SignalNumber int
	Failures     Failure
}

// Run starts executable directly, without a shell. It forwards stdin to the
// child and copies stdout and stderr byte-for-byte to their respective writers.
// It waits for all output forwarding to finish, even after cancellation. A nil
// stdin gives the child immediate EOF. A nil output writer discards that stream
// while still counting it. Calls to the same comparable writer supplied for
// both output streams are serialized.
//
// Cancellation terminates the supervised platform process group or job. On
// Unix, that includes descendants that remain in the child's process group;
// portable process groups cannot contain a descendant that deliberately moves
// itself into a different group or session.
//
// Run does not retain output, argument values, environment variables, or
// errors that might contain those values. Output statistics describe bytes
// read from the child, including bytes discarded after a downstream writer
// fails.
func Run(
	ctx context.Context,
	executable string,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) Result {
	result := Result{
		Command: CommandMetadata{
			Executable:    executableBase(executable),
			ArgumentCount: len(args),
		},
		ExitCode: -1,
	}

	if err := ctx.Err(); err != nil {
		result.Failures |= contextFailure(err)
		return result
	}

	stdoutCounter := newStreamCounter(stdout)
	stderrCounter := newStreamCounter(stderr)
	if writersEqual(stdout, stderr) {
		// Wrapping the destinations in separate counters hides their identity
		// from os/exec. Restore its shared-writer serialization guarantee so a
		// comparable writer supplied for both streams is never called
		// concurrently.
		sharedDestination := new(sync.Mutex)
		stdoutCounter.destinationMu = sharedDestination
		stderrCounter.destinationMu = sharedDestination
	}
	cmd := exec.Command(executable, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdoutCounter
	cmd.Stderr = stderrCounter

	process := prepareProcess(cmd)
	if err := cmd.Start(); err != nil {
		process.close()
		result.Failures |= FailureStart
		return result
	}

	if err := process.afterStart(cmd); err != nil {
		// Windows starts the child suspended until tree supervision is ready.
		// On every platform, make sure a partially initialized child is reaped.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		process.close()
		result.Stdout = stdoutCounter.stats()
		result.Stderr = stderrCounter.stats()
		result.Failures |= FailureStart
		if result.Stdout.CopyFailed || result.Stderr.CopyFailed {
			result.Failures |= FailureOutputCopy
		}
		return result
	}

	waited := make(chan error, 1)
	go func() {
		waited <- cmd.Wait()
	}()

	var (
		waitErr         error
		contextFinished bool
	)
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		// Prefer a completed Wait when command completion and cancellation race.
		select {
		case waitErr = <-waited:
		default:
			contextFinished = true
			_ = process.terminate(cmd)
			// There is deliberately no timeout here. Wait includes os/exec's output
			// copy goroutines, so returning early could truncate buffered output.
			waitErr = <-waited
		}
	}
	process.close()

	result.Stdout = stdoutCounter.stats()
	result.Stderr = stderrCounter.stats()
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}

	if contextFinished {
		result.Failures |= contextFailure(ctx.Err())
	} else if waitErr != nil {
		var exitErr *exec.ExitError
		if !asExitError(waitErr, &exitErr) {
			// Downstream write errors are absorbed so the pipe can be drained. A
			// remaining non-exit Wait error is therefore an OS-level copy failure.
			result.Failures |= FailureOutputCopy
		} else if name, number, signaled := processSignal(cmd.ProcessState); signaled {
			result.Signal = name
			result.SignalNumber = number
			result.Failures |= FailureSignal
		}
	}

	if result.Stdout.CopyFailed || result.Stderr.CopyFailed {
		result.Failures |= FailureOutputCopy
	}
	return result
}

func executableBase(executable string) string {
	if executable == "" {
		return ""
	}
	return filepath.Base(executable)
}

func contextFailure(err error) Failure {
	if err == context.DeadlineExceeded {
		return FailureTimeout
	}
	return FailureCanceled
}

// asExitError is kept small so Result never stores the possibly more detailed
// error returned by os/exec.
func asExitError(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError)
	if ok {
		*target = exitErr
	}
	return ok
}

type streamCounter struct {
	destination   io.Writer
	destinationMu *sync.Mutex
	bytes         uint64
	lines         uint64
	failed        bool
}

func newStreamCounter(destination io.Writer) *streamCounter {
	if destination == nil {
		destination = io.Discard
	}
	return &streamCounter{destination: destination}
}

func (w *streamCounter) Write(p []byte) (int, error) {
	w.bytes += uint64(len(p))
	for _, b := range p {
		if b == '\n' {
			w.lines++
		}
	}

	if !w.failed {
		w.writeAll(p)
	}

	// Always report the full input as consumed. Once a destination fails, this
	// drains the child's pipe without retaining output or allowing it to block.
	return len(p), nil
}

func (w *streamCounter) writeAll(p []byte) {
	if w.destinationMu != nil {
		w.destinationMu.Lock()
		defer w.destinationMu.Unlock()
	}

	for len(p) > 0 {
		n, err := w.destination.Write(p)
		if n < 0 || n > len(p) {
			w.failed = true
			return
		}
		p = p[n:]
		if err != nil || n == 0 {
			w.failed = true
			return
		}
	}
}

// writersEqual mirrors os/exec's guarded interface comparison. An io.Writer
// can have a non-comparable dynamic value, in which case comparing the two
// interfaces directly would panic.
func writersEqual(a, b io.Writer) (equal bool) {
	if a == nil || b == nil {
		return false
	}
	defer func() {
		_ = recover()
	}()
	return a == b
}

func (w *streamCounter) stats() StreamStats {
	return StreamStats{
		Bytes:      w.bytes,
		Lines:      w.lines,
		CopyFailed: w.failed,
	}
}
