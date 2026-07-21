// Package statepath resolves the one state directory used by the CLI.
package statepath

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/motus-os/work-ledger/internal/gitmeta"
)

const EnvironmentVariable = "MOTUS_STATE_DIR"

// Resolve chooses an explicit path first, then an environment path, then a
// .motus directory at the current Git root or working directory. Relative
// paths are interpreted from cwd.
func Resolve(ctx context.Context, cwd, explicit, environment string) (string, error) {
	if cwd == "" {
		return "", fmt.Errorf("working directory is required")
	}
	if explicit != "" {
		return absoluteFrom(cwd, explicit)
	}
	if environment != "" {
		return absoluteFrom(cwd, environment)
	}
	metadata := gitmeta.Detect(ctx, cwd)
	base := cwd
	if metadata.Root != "" {
		base = metadata.Root
	}
	return filepath.Join(base, ".motus"), nil
}

func absoluteFrom(cwd, value string) (string, error) {
	if !filepath.IsAbs(value) {
		value = filepath.Join(cwd, value)
	}
	abs, err := filepath.Abs(filepath.Clean(value))
	if err != nil {
		return "", fmt.Errorf("resolve state directory: %w", err)
	}
	return abs, nil
}
