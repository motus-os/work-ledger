//go:build unix

package signals

import (
	"os"
	"os/signal"
	"syscall"
)

// IgnoreBrokenPipe makes downstream pipe closure observable as a write error.
// That lets Motus finish draining the child and close its run record instead
// of being terminated before lifecycle cleanup runs.
func IgnoreBrokenPipe() {
	signal.Ignore(syscall.SIGPIPE)
}

func Process() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
