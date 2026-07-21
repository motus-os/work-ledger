//go:build !windows

package store

func validatePlatformStatePath(string) error { return nil }
