//go:build windows

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsStatePathRejectsNetworkAndDevicePaths(t *testing.T) {
	for _, path := range []string{`\\server\share\state`, `\\?\C:\state`, `\\.\C:\state`} {
		if err := validatePlatformStatePath(path); !errors.Is(err, ErrUnsafeStatePath) {
			t.Fatalf("validatePlatformStatePath(%q) error = %v", path, err)
		}
	}
	if err := validatePlatformStatePath(`C:\Users\Example\state`); err != nil {
		t.Fatalf("validatePlatformStatePath(local drive) error = %v", err)
	}
}

func TestOpenCreatesLedgerAtWindowsLocalPath(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "project state", "ledger state")
	ledger, err := Open(context.Background(), stateDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, DatabaseFilename)); err != nil {
		t.Fatalf("Stat(database) error = %v", err)
	}
}
