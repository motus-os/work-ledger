// Package buildinfo exposes release metadata injected by the build.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func EffectiveVersion() string {
	if Version != "" && Version != "dev" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" &&
		!strings.Contains(info.Main.Version, "+dirty") {
		return info.Main.Version
	}
	return "dev"
}
