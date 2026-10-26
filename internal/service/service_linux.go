//go:build linux

package service

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// HasPackageManager reports whether apt, dnf or yum is installed (the
// package update check needs one).
func HasPackageManager() bool {
	for _, name := range []string{"apt-get", "dnf", "yum"} {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
	}
	return false
}

// Install writes the systemd unit, then enables and starts it. The service
// user must exist (install.sh creates it).
func Install(o Options) error {
	if os.Geteuid() != 0 {
		return errors.New("install must run as root")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemd not found; run `spectate-agent run` under your init system")
	}
	if err := writeUnits(o); err != nil {
		return err
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := run("systemctl", "enable", "--now", Name); err != nil {
		return err
	}
	return enableTimers(o)
}

func writeUnits(o Options) error {
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
	if o.Packages {
		svc, timer, err := PackagesUnits(o)
		if err != nil {
			return err
		}
		if err := os.WriteFile(PackagesServicePath, []byte(svc), 0o644); err != nil {
			return fmt.Errorf("write package check unit: %w", err)
		}
		if err := os.WriteFile(PackagesTimerPath, []byte(timer), 0o644); err != nil {
			return fmt.Errorf("write package check timer: %w", err)
		}
	}
	return nil
}

func enableTimers(o Options) error {
	if o.AutoUpdate {
		if err := run("systemctl", "enable", "--now", UpdateName+".timer"); err != nil {
			return err
		}
	}
	if o.Packages {
		return run("systemctl", "enable", "--now", PackagesName+".timer")
	}
	return nil
}

// RefreshUnits rewrites the units of an installed agent with this binary's
// templates (after an update, so existing hosts get new units), keeping its
// user, environment file and auto-update timer, and adding the package
// update check where a package manager is installed. The agent service is
// not restarted; the caller does that.
func RefreshUnits(binary string) error {
	if os.Geteuid() != 0 {
		return errors.New("refreshing the units must run as root")
	}
	current, err := os.ReadFile(UnitPath)
	if err != nil {
		return err
	}
	o := Options{
		Binary:     binary,
		User:       unitValue(current, "User="),
		EnvFile:    strings.TrimPrefix(unitValue(current, "EnvironmentFile="), "-"),
		AutoUpdate: fileExists(UpdateTimerPath),
		Packages:   HasPackageManager(),
	}
	if err := writeUnits(o); err != nil {
		return err
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	return enableTimers(o)
}

func unitValue(unit []byte, prefix string) string {
	sc := bufio.NewScanner(bytes.NewReader(unit))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), prefix); ok {
			return v
		}
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Uninstall stops and removes the units, including the auto-update and
// package check timers (state in /var/lib is kept).
func Uninstall() error {
	if os.Geteuid() != 0 {
		return errors.New("uninstall must run as root")
	}
	if _, err := os.Stat(UpdateTimerPath); err == nil {
		_ = run("systemctl", "disable", "--now", UpdateName+".timer")
	}
	if _, err := os.Stat(PackagesTimerPath); err == nil {
		_ = run("systemctl", "disable", "--now", PackagesName+".timer")
	}
	_ = run("systemctl", "disable", "--now", Name)
	for _, path := range []string{UnitPath, UpdateServicePath, UpdateTimerPath, PackagesServicePath, PackagesTimerPath} {
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
