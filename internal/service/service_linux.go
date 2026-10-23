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
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	return run("systemctl", "enable", "--now", Name)
}

// Uninstall stops and removes the unit (state in /var/lib is kept).
func Uninstall() error {
	if os.Geteuid() != 0 {
		return errors.New("uninstall must run as root")
	}
	_ = run("systemctl", "disable", "--now", Name)
	if err := os.Remove(UnitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return run("systemctl", "daemon-reload")
}
