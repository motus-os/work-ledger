package signals

import (
	"context"
	"testing"
)

func TestCancellationNumber(t *testing.T) {
	ordinary, ordinaryCancel := context.WithCancel(context.Background())
	ordinaryCancel()
	if got := CancellationNumber(ordinary); got != 0 {
		t.Fatalf("ordinary cancellation number = %d, want 0", got)
	}

	signaled, signalCancel := context.WithCancelCause(context.Background())
	signalCancel(processSignalCause{number: 15})
	if got := CancellationNumber(signaled); got != 15 {
		t.Fatalf("signal cancellation number = %d, want 15", got)
	}
}
