//go:build linux

package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// Install writes the systemd unit, then enables and starts it. The service
// user must exist (install.sh creates it).
func Install(o Options) error {
	if os.Geteuid() != 0 {
		return errors.New("install must run as root")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemd not found; run `spectate-agent run` under your init system")
	}
	unit, err := Unit(o)
	if err != nil {
		return err
	}
	if err := os.WriteFile(UnitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if o.AutoUpdate {
		svc, timer, err := UpdateUnits(o)
		if err != nil {
			return err
		}
		if err := os.WriteFile(UpdateServicePath, []byte(svc), 0o644); err != nil {
			return fmt.Errorf("write update unit: %w", err)
		}
		if err := os.WriteFile(UpdateTimerPath, []byte(timer), 0o644); err != nil {
			return fmt.Errorf("write update timer: %w", err)
		}
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := run("systemctl", "enable", "--now", Name); err != nil {
		return err
	}
	if o.AutoUpdate {
		return run("systemctl", "enable", "--now", UpdateName+".timer")
	}
	return nil
}

// Uninstall stops and removes the units, including the auto-update timer
// (state in /var/lib is kept).
func Uninstall() error {
	if os.Geteuid() != 0 {
		return errors.New("uninstall must run as root")
	}
	if _, err := os.Stat(UpdateTimerPath); err == nil {
		_ = run("systemctl", "disable", "--now", UpdateName+".timer")
	}
	_ = run("systemctl", "disable", "--now", Name)
	for _, path := range []string{UnitPath, UpdateServicePath, UpdateTimerPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return run("systemctl", "daemon-reload")
}

// Installed reports whether the systemd unit is installed.
func Installed() bool {
	_, err := os.Stat(UnitPath)
	return err == nil
}

// Restart restarts the installed service. With onlyIfRunning a stopped (or
// failed) service is left alone (systemctl try-restart).
func Restart(onlyIfRunning bool) error {
	if onlyIfRunning {
		return run("systemctl", "try-restart", Name)
	}
	return run("systemctl", "restart", Name)
}

// RestartHint returns the command that restarts the service.
func RestartHint() string { return "sudo systemctl restart " + Name }
