//go:build !linux && !darwin

package service

import "errors"

// Install is not supported on this platform.
func Install(Options) error {
	return errors.New("service install is only supported on Linux and macOS")
}

// Uninstall is not supported on this platform.
func Uninstall() error { return errors.New("service uninstall is only supported on Linux and macOS") }

// Installed reports whether the service is installed (never, here).
func Installed() bool { return false }

// Restart is not supported on this platform.
func Restart(bool) error { return errors.New("service restart is only supported on Linux") }

// RestartHint says how to restart the agent here.
func RestartHint() string { return "restart spectate-agent under your init system" }
