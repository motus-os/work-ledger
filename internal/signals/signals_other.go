//go:build !unix

package signals

import "os"

func IgnoreBrokenPipe() {}

func processSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func signalNumber(received os.Signal) int {
	if received == os.Interrupt {
		return 2
	}
	return 0
}
