//go:build !linux && !darwin

package service

import "errors"

// Install is not supported on this platform.
func Install(Options) error {
	return errors.New("service install is only supported on Linux and macOS")
}

// Uninstall is not supported on this platform.
func Uninstall() error { return errors.New("service uninstall is only supported on Linux and macOS") }
