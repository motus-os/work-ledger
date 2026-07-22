package signals

import (
	"context"
	"os"
	"os/signal"
)

// NotifyContext returns a context canceled when the process receives one of
// the configured platform signals. The cancellation cause retains
// only the numeric signal so callers can preserve conventional shell exit
// status without retaining operating-system error text.
func NotifyContext(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	notifications := make(chan os.Signal, 1)
	signal.Notify(notifications, processSignals()...)

	go func() {
		select {
		case received := <-notifications:
			// Restore the operating system's default behavior immediately. A
			// second signal should not be hidden while the first is closing the
			// recorded run.
			signal.Stop(notifications)
			cancel(processSignalCause{number: signalNumber(received)})
		case <-ctx.Done():
		}
	}()

	return ctx, func() {
		signal.Stop(notifications)
		cancel(context.Canceled)
	}
}

// CancellationNumber returns the process signal associated with a context
// cancellation, or zero for ordinary cancellation and deadlines.
func CancellationNumber(ctx context.Context) int {
	cause, ok := context.Cause(ctx).(interface{ SignalNumber() int })
	if !ok {
		return 0
	}
	return cause.SignalNumber()
}

type processSignalCause struct {
	number int
}

func (processSignalCause) Error() string {
	return "process signal"
}

func (cause processSignalCause) SignalNumber() int {
	return cause.number
}
