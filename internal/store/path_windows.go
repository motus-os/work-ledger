//go:build windows

package store

import (
	"fmt"
	"path/filepath"
	"strings"
)

func validatePlatformStatePath(path string) error {
	volume := filepath.VolumeName(path)
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(volume, `\\`) {
		return fmt.Errorf("%w: Windows network and device paths are not supported", ErrUnsafeStatePath)
	}
	return nil
}
