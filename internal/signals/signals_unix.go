//go:build unix

package signals

import (
	"os"
	"os/signal"
	"syscall"
)

// IgnoreBrokenPipe makes downstream pipe closure observable as a write error.
// That lets Motus stop the supervised child tree and close its run record
// instead of being terminated before lifecycle cleanup runs.
func IgnoreBrokenPipe() {
	signal.Ignore(syscall.SIGPIPE)
}

func processSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

func signalNumber(received os.Signal) int {
	value, ok := received.(syscall.Signal)
	if !ok {
		return 0
	}
	return int(value)
}
