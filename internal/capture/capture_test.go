package capture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	captureHelperMarker     = "capture-test-helper"
	largeOutputSize         = 1024*1024 + 65_537
	simultaneousStdoutSize  = 1024*1024 + 131_071
	simultaneousStderrSize  = 1024*1024 + 262_147
	helperInternalFailure   = 120
	helperNonzeroExit       = 23
	helperBlockingSleepTime = 30 * time.Second
)

var (
	exactStdout = []byte("stdout: first line\nstdout: second line\nunterminated\x00\xff")
	exactStderr = []byte("\x00stderr: first line\n\nstderr: unterminated")
	exitStdout  = []byte("stdout before nonzero exit\n")
	exitStderr  = []byte("stderr before nonzero exit\n")
	blockStdout = []byte("child-started\nchild-ready\n")
	blockStderr = []byte("child-stderr-ready\n")
)

// TestCaptureHelperProcess is re-executed by the tests below so the package
// can exercise os/exec without relying on a platform-specific shell or
// utility. A normal test invocation does not contain the helper marker and
// returns immediately.
func TestCaptureHelperProcess(t *testing.T) {
	args, ok := testHelperInvocation(captureHelperMarker)
	if !ok {
		return
	}
	os.Exit(runCaptureHelper(args))
}

func runCaptureHelper(args []string) int {
	if len(args) == 0 {
		return helperInternalFailure
	}

	switch args[0] {
	case "exact":
		if writeHelperOutput(os.Stdout, exactStdout) != nil ||
			writeHelperOutput(os.Stderr, exactStderr) != nil {
			return helperInternalFailure
		}
		return 0
	case "generated":
		if len(args) != 3 {
			return helperInternalFailure
		}
		size, err := strconv.Atoi(args[1])
		if err != nil || size < 0 {
			return helperInternalFailure
		}
		seed, err := strconv.Atoi(args[2])
		if err != nil || seed < 0 || seed > 255 {
			return helperInternalFailure
		}
		if writeHelperOutput(os.Stdout, generatedOutput(size, byte(seed))) != nil {
			return helperInternalFailure
		}
		return 0
	case "simultaneous":
		stdout := generatedOutput(simultaneousStdoutSize, 17)
		stderr := generatedOutput(simultaneousStderrSize, 83)
		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			errs <- writeHelperOutput(os.Stdout, stdout)
		}()
		go func() {
			<-start
			errs <- writeHelperOutput(os.Stderr, stderr)
		}()
		close(start)
		stdoutErr := <-errs
		stderrErr := <-errs
		if stdoutErr != nil || stderrErr != nil {
			return helperInternalFailure
		}
		return 0
	case "block":
		// Write stdout last so observing child-ready also proves that the child
		// attempted all expected stderr output before cancellation.
		if writeHelperOutput(os.Stderr, blockStderr) != nil ||
			writeHelperOutput(os.Stdout, blockStdout) != nil {
			return helperInternalFailure
		}
		time.Sleep(helperBlockingSleepTime)
		return 0
	case "exit-nonzero":
		if writeHelperOutput(os.Stdout, exitStdout) != nil ||
			writeHelperOutput(os.Stderr, exitStderr) != nil {
			return helperInternalFailure
		}
		return helperNonzeroExit
	case "metadata":
		// Additional arguments are deliberately accepted and ignored.
		return 0
	case "stdin":
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			return helperInternalFailure
		}
		return 0
	default:
		return helperInternalFailure
	}
}

func TestRunForwardsAndCountsExactStreams(t *testing.T) {
	stdout := &shortWriter{maximum: 7}
	stderr := &shortWriter{maximum: 5}
	args := captureHelperArgs("exact")

	result := Run(context.Background(), os.Args[0], args, nil, stdout, stderr)

	assertHelperMetadata(t, result, len(args))
	assertResultOutcome(t, result, 0, 0)
	assertBytesEqual(t, "stdout", stdout.Bytes(), exactStdout)
	assertBytesEqual(t, "stderr", stderr.Bytes(), exactStderr)
	assertStreamStats(t, "stdout", result.Stdout, exactStdout, false)
	assertStreamStats(t, "stderr", result.Stderr, exactStderr, false)
}

func TestRunForwardsStdinWithoutRetainingIt(t *testing.T) {
	const privateInput = "stdin remains process input only\nsecond line\n"
	var stdout bytes.Buffer
	args := captureHelperArgs("stdin")

	result := Run(
		context.Background(),
		os.Args[0],
		args,
		strings.NewReader(privateInput),
		&stdout,
		nil,
	)

	assertResultOutcome(t, result, 0, 0)
	assertBytesEqual(t, "stdout", stdout.Bytes(), []byte(privateInput))
	assertStreamStats(t, "stdout", result.Stdout, []byte(privateInput), false)
	if strings.Contains(fmt.Sprintf("%#v", result), privateInput) {
		t.Fatal("Result retained stdin content")
	}
}

func TestRunDoesNotTruncateOutputLargerThanOneMiB(t *testing.T) {
	expected := generatedOutput(largeOutputSize, 41)
	var stdout bytes.Buffer
	args := captureHelperArgs("generated", strconv.Itoa(len(expected)), "41")

	result := Run(context.Background(), os.Args[0], args, nil, &stdout, nil)

	assertResultOutcome(t, result, 0, 0)
	assertBytesEqual(t, "stdout", stdout.Bytes(), expected)
	assertStreamStats(t, "stdout", result.Stdout, expected, false)
	assertStreamStats(t, "stderr", result.Stderr, nil, false)
}

func TestRunForwardsSimultaneousStreamsExactly(t *testing.T) {
	expectedStdout := generatedOutput(simultaneousStdoutSize, 17)
	expectedStderr := generatedOutput(simultaneousStderrSize, 83)
	var stdout, stderr bytes.Buffer

	result := Run(
		context.Background(),
		os.Args[0],
		captureHelperArgs("simultaneous"),
		nil,
		&stdout,
		&stderr,
	)

	assertResultOutcome(t, result, 0, 0)
	assertBytesEqual(t, "stdout", stdout.Bytes(), expectedStdout)
	assertBytesEqual(t, "stderr", stderr.Bytes(), expectedStderr)
	assertStreamStats(t, "stdout", result.Stdout, expectedStdout, false)
	assertStreamStats(t, "stderr", result.Stderr, expectedStderr, false)
}

func TestRunSerializesComparableSharedWriter(t *testing.T) {
	expectedStdout := generatedOutput(simultaneousStdoutSize, 17)
	expectedStderr := generatedOutput(simultaneousStderrSize, 83)
	shared := new(concurrencyCheckingWriter)

	result := Run(
		context.Background(),
		os.Args[0],
		captureHelperArgs("simultaneous"),
		nil,
		shared,
		shared,
	)

	assertResultOutcome(t, result, 0, 0)
	assertStreamStats(t, "stdout", result.Stdout, expectedStdout, false)
	assertStreamStats(t, "stderr", result.Stderr, expectedStderr, false)
	if got := shared.MaximumConcurrency(); got != 1 {
		t.Fatalf("shared writer maximum concurrency = %d, want 1", got)
	}
	if got, want := len(shared.Bytes()), len(expectedStdout)+len(expectedStderr); got != want {
		t.Fatalf("shared writer byte count = %d, want %d", got, want)
	}
}

func TestRunWaitsForSlowWriterWithoutBlockingOtherStream(t *testing.T) {
	expectedStdout := generatedOutput(simultaneousStdoutSize, 17)
	expectedStderr := generatedOutput(simultaneousStderrSize, 83)
	entered := make(chan struct{})
	release := make(chan struct{})
	writer := &gatedWriter{entered: entered, release: release}
	stderr := newCompletionBuffer(len(expectedStderr))
	ctx, cancel := context.WithCancel(context.Background())
	resultc := make(chan Result, 1)
	released := false
	defer func() {
		cancel()
		if !released {
			close(release)
		}
	}()

	go func() {
		resultc <- Run(
			ctx,
			os.Args[0],
			captureHelperArgs("simultaneous"),
			nil,
			writer,
			stderr,
		)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child output did not reach the slow writer")
	}
	select {
	case <-stderr.Complete():
	case <-time.After(5 * time.Second):
		t.Fatal("slow stdout writer blocked independent stderr forwarding")
	}
	select {
	case result := <-resultc:
		close(release)
		released = true
		t.Fatalf("Run returned before slow writer was released: %+v", result)
	case <-time.After(75 * time.Millisecond):
	}

	close(release)
	released = true
	result := awaitResult(t, resultc, 5*time.Second)
	assertResultOutcome(t, result, 0, 0)
	assertBytesEqual(t, "slow stdout", writer.Bytes(), expectedStdout)
	assertBytesEqual(t, "concurrent stderr", stderr.Bytes(), expectedStderr)
	assertStreamStats(t, "stdout", result.Stdout, expectedStdout, false)
	assertStreamStats(t, "stderr", result.Stderr, expectedStderr, false)
}

func TestRunDrainsAndCountsAfterWriterFailure(t *testing.T) {
	const writerSecret = "writer-error-secret-must-not-be-retained"
	expectedStdout := generatedOutput(simultaneousStdoutSize, 17)
	expectedStderr := generatedOutput(simultaneousStderrSize, 83)
	failed := &failAfterWriter{
		limit: 137,
		err:   errors.New(writerSecret),
	}
	var stderr bytes.Buffer

	result := Run(
		context.Background(),
		os.Args[0],
		captureHelperArgs("simultaneous"),
		nil,
		failed,
		&stderr,
	)

	assertResultOutcome(t, result, 0, FailureOutputCopy)
	assertBytesEqual(t, "forwarded stdout prefix", failed.Bytes(), expectedStdout[:failed.limit])
	assertBytesEqual(t, "stderr after stdout failure", stderr.Bytes(), expectedStderr)
	assertStreamStats(t, "stdout", result.Stdout, expectedStdout, true)
	assertStreamStats(t, "stderr", result.Stderr, expectedStderr, false)
	if got := failed.WritesAfterFailure(); got != 0 {
		t.Fatalf("destination received %d writes after reporting failure", got)
	}
	if strings.Contains(fmt.Sprintf("%#v", result), writerSecret) {
		t.Fatal("Result retained the downstream writer error text")
	}
}

func TestRunTreatsNonzeroExitAsOrdinaryOutcome(t *testing.T) {
	var stdout, stderr bytes.Buffer
	result := Run(
		context.Background(),
		os.Args[0],
		captureHelperArgs("exit-nonzero"),
		nil,
		&stdout,
		&stderr,
	)

	assertResultOutcome(t, result, helperNonzeroExit, 0)
	assertBytesEqual(t, "stdout", stdout.Bytes(), exitStdout)
	assertBytesEqual(t, "stderr", stderr.Bytes(), exitStderr)
	assertStreamStats(t, "stdout", result.Stdout, exitStdout, false)
	assertStreamStats(t, "stderr", result.Stderr, exitStderr, false)
}

func TestRunReportsStartFailureWithoutLeakingPathOrArguments(t *testing.T) {
	const argumentSecret = "start-argument-secret-must-not-be-retained"
	privateDirectory := filepath.Join(t.TempDir(), "private-directory-must-not-be-retained")
	executable := filepath.Join(privateDirectory, "capture-definitely-missing")
	var stdout, stderr bytes.Buffer

	result := Run(
		context.Background(),
		executable,
		[]string{argumentSecret},
		nil,
		&stdout,
		&stderr,
	)

	assertResultOutcome(t, result, -1, FailureStart)
	if got, want := result.Command.Executable, filepath.Base(executable); got != want {
		t.Fatalf("executable metadata = %q, want %q", got, want)
	}
	if result.Command.ArgumentCount != 1 {
		t.Fatalf("argument count = %d, want 1", result.Command.ArgumentCount)
	}
	assertStreamStats(t, "stdout", result.Stdout, nil, false)
	assertStreamStats(t, "stderr", result.Stderr, nil, false)
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("start failure forwarded output: stdout=%d stderr=%d", stdout.Len(), stderr.Len())
	}
	dump := fmt.Sprintf("%#v", result)
	for _, secret := range []string{argumentSecret, privateDirectory} {
		if strings.Contains(dump, secret) {
			t.Fatalf("Result retained private start data %q", secret)
		}
	}
}

func TestRunCancelsRunningProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stdout := newSignalBuffer([]byte("child-ready\n"))
	var stderr bytes.Buffer
	resultc := make(chan Result, 1)
	defer cancel()

	go func() {
		resultc <- Run(ctx, os.Args[0], captureHelperArgs("block"), nil, stdout, &stderr)
	}()

	select {
	case <-stdout.Ready():
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("child did not become ready for cancellation")
	}
	cancel()
	result := awaitResult(t, resultc, 5*time.Second)

	assertResultOutcome(t, result, result.ExitCode, FailureCanceled)
	if result.ExitCode == 0 {
		t.Fatal("canceled process reported a successful exit code")
	}
	stdoutBytes := stdout.Bytes()
	assertBytesEqual(t, "stdout", stdoutBytes, blockStdout)
	assertBytesEqual(t, "stderr", stderr.Bytes(), blockStderr)
	assertStreamStats(t, "stdout", result.Stdout, stdoutBytes, false)
	assertStreamStats(t, "stderr", result.Stderr, blockStderr, false)
}

func TestRunReportsTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stdout := newSignalBuffer([]byte("child-ready\n"))
	var stderr bytes.Buffer

	result := Run(ctx, os.Args[0], captureHelperArgs("block"), nil, stdout, &stderr)

	assertResultOutcome(t, result, result.ExitCode, FailureTimeout)
	if result.ExitCode == 0 {
		t.Fatal("timed-out process reported a successful exit code")
	}
	select {
	case <-stdout.Ready():
	default:
		t.Fatal("timeout elapsed before the helper process became ready")
	}
	stdoutBytes := stdout.Bytes()
	assertBytesEqual(t, "stdout", stdoutBytes, blockStdout)
	assertBytesEqual(t, "stderr", stderr.Bytes(), blockStderr)
	assertStreamStats(t, "stdout", result.Stdout, stdoutBytes, false)
	assertStreamStats(t, "stderr", result.Stderr, blockStderr, false)
}

func TestRunDoesNotStartWithCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	args := captureHelperArgs("block")

	result := Run(ctx, os.Args[0], args, nil, nil, nil)

	assertHelperMetadata(t, result, len(args))
	assertResultOutcome(t, result, -1, FailureCanceled)
	assertStreamStats(t, "stdout", result.Stdout, nil, false)
	assertStreamStats(t, "stderr", result.Stderr, nil, false)
}

func TestResultMetadataDoesNotRetainArgumentValues(t *testing.T) {
	const (
		secretOne = "argument-value-secret-0678e148"
		secretTwo = "--token=second-secret-f9d216cb"
	)
	args := captureHelperArgs("metadata", secretOne, secretTwo)

	result := Run(context.Background(), os.Args[0], args, nil, nil, nil)

	assertHelperMetadata(t, result, len(args))
	assertResultOutcome(t, result, 0, 0)
	dump := fmt.Sprintf("%#v", result)
	for _, secret := range []string{secretOne, secretTwo} {
		if strings.Contains(dump, secret) {
			t.Fatalf("Result retained argument value %q", secret)
		}
	}
}

func captureHelperArgs(mode string, extra ...string) []string {
	args := []string{
		"-test.run=^TestCaptureHelperProcess$",
		"--",
		captureHelperMarker,
		mode,
	}
	return append(args, extra...)
}

func testHelperInvocation(marker string) ([]string, bool) {
	for i := 1; i+2 < len(os.Args); i++ {
		if os.Args[i] == "--" && os.Args[i+1] == marker {
			return os.Args[i+2:], true
		}
	}
	return nil, false
}

func writeHelperOutput(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n < 0 || n > len(p) {
			return io.ErrShortWrite
		}
		p = p[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func generatedOutput(size int, seed byte) []byte {
	output := make([]byte, size)
	for i := range output {
		switch {
		case i%257 == 0:
			output[i] = '\n'
		case i%509 == 0:
			output[i] = 0
		default:
			output[i] = byte((i*37 + int(seed)) % 251)
		}
	}
	return output
}

func assertHelperMetadata(t *testing.T, result Result, argumentCount int) {
	t.Helper()
	if got, want := result.Command.Executable, filepath.Base(os.Args[0]); got != want {
		t.Fatalf("executable metadata = %q, want %q", got, want)
	}
	if got := result.Command.ArgumentCount; got != argumentCount {
		t.Fatalf("argument count = %d, want %d", got, argumentCount)
	}
}

func assertResultOutcome(t *testing.T, result Result, exitCode int, failures Failure) {
	t.Helper()
	if result.ExitCode != exitCode {
		t.Errorf("exit code = %d, want %d", result.ExitCode, exitCode)
	}
	if result.Failures != failures {
		t.Errorf("failures = %05b, want %05b", result.Failures, failures)
	}
}

func assertStreamStats(
	t *testing.T,
	name string,
	stats StreamStats,
	observed []byte,
	copyFailed bool,
) {
	t.Helper()
	wantBytes := uint64(len(observed))
	wantLines := uint64(bytes.Count(observed, []byte{'\n'}))
	if stats.Bytes != wantBytes || stats.Lines != wantLines || stats.CopyFailed != copyFailed {
		t.Errorf(
			"%s stats = {Bytes:%d Lines:%d CopyFailed:%t}, want {Bytes:%d Lines:%d CopyFailed:%t}",
			name,
			stats.Bytes,
			stats.Lines,
			stats.CopyFailed,
			wantBytes,
			wantLines,
			copyFailed,
		)
	}
}

func assertBytesEqual(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	limit := min(len(got), len(want))
	firstDifference := limit
	for i := 0; i < limit; i++ {
		if got[i] != want[i] {
			firstDifference = i
			break
		}
	}
	t.Fatalf(
		"%s differs: got %d bytes, want %d bytes, first difference at byte %d",
		name,
		len(got),
		len(want),
		firstDifference,
	)
}

func awaitResult(t *testing.T, resultc <-chan Result, timeout time.Duration) Result {
	t.Helper()
	select {
	case result := <-resultc:
		return result
	case <-time.After(timeout):
		t.Fatalf("Run did not return within %s", timeout)
		return Result{}
	}
}

type shortWriter struct {
	maximum int
	buffer  bytes.Buffer
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.maximum {
		p = p[:w.maximum]
	}
	return w.buffer.Write(p)
}

func (w *shortWriter) Bytes() []byte {
	return w.buffer.Bytes()
}

type concurrencyCheckingWriter struct {
	active   atomic.Int32
	maximum  atomic.Int32
	bufferMu sync.Mutex
	buffer   bytes.Buffer
}

func (w *concurrencyCheckingWriter) Write(p []byte) (int, error) {
	active := w.active.Add(1)
	defer w.active.Add(-1)
	for {
		maximum := w.maximum.Load()
		if active <= maximum || w.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}

	// Widen the overlap window so a missing shared-writer lock is reliably
	// detected even on a single-core builder.
	time.Sleep(250 * time.Microsecond)
	w.bufferMu.Lock()
	defer w.bufferMu.Unlock()
	return w.buffer.Write(p)
}

func (w *concurrencyCheckingWriter) MaximumConcurrency() int32 {
	return w.maximum.Load()
}

func (w *concurrencyCheckingWriter) Bytes() []byte {
	w.bufferMu.Lock()
	defer w.bufferMu.Unlock()
	return bytes.Clone(w.buffer.Bytes())
}

type gatedWriter struct {
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
	mu      sync.Mutex
	buffer  bytes.Buffer
}

type completionBuffer struct {
	target   int
	complete chan struct{}
	once     sync.Once
	mu       sync.Mutex
	buffer   bytes.Buffer
}

func newCompletionBuffer(target int) *completionBuffer {
	return &completionBuffer{
		target:   target,
		complete: make(chan struct{}),
	}
}

func (w *completionBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buffer.Write(p)
	if w.buffer.Len() >= w.target {
		w.once.Do(func() { close(w.complete) })
	}
	return n, err
}

func (w *completionBuffer) Complete() <-chan struct{} {
	return w.complete
}

func (w *completionBuffer) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buffer.Bytes())
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.Write(p)
}

func (w *gatedWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buffer.Bytes())
}

type failAfterWriter struct {
	limit              int
	err                error
	buffer             bytes.Buffer
	failed             bool
	writesAfterFailure int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.failed {
		w.writesAfterFailure++
		return 0, w.err
	}
	remaining := w.limit - w.buffer.Len()
	if remaining < len(p) {
		p = p[:remaining]
	}
	n, _ := w.buffer.Write(p)
	if w.buffer.Len() == w.limit {
		w.failed = true
		return n, w.err
	}
	return n, nil
}

func (w *failAfterWriter) Bytes() []byte {
	return w.buffer.Bytes()
}

func (w *failAfterWriter) WritesAfterFailure() int {
	return w.writesAfterFailure
}

type signalBuffer struct {
	needle []byte
	ready  chan struct{}
	once   sync.Once
	mu     sync.Mutex
	buffer bytes.Buffer
}

func newSignalBuffer(needle []byte) *signalBuffer {
	return &signalBuffer{
		needle: bytes.Clone(needle),
		ready:  make(chan struct{}),
	}
}

func (w *signalBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buffer.Write(p)
	if bytes.Contains(w.buffer.Bytes(), w.needle) {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}

func (w *signalBuffer) Ready() <-chan struct{} {
	return w.ready
}

func (w *signalBuffer) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buffer.Bytes())
}
