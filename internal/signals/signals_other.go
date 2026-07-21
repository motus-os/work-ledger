//go:build !unix

package signals

import "os"

func IgnoreBrokenPipe() {}

func Process() []os.Signal {
	return []os.Signal{os.Interrupt}
}
