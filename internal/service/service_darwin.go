//go:build darwin

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

var plistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>net.spectate.agent</string>
  <key>ProgramArguments</key>
  <array><string>{{.Binary}}</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardErrorPath</key><string>{{.Log}}</string>
</dict>
</plist>
`))

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", "net.spectate.agent.plist"), nil
}

// Install writes a per-user launchd agent and loads it (best effort on macOS).
func Install(o Options) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := plistTemplate.Execute(f, map[string]string{
		"Binary": o.Binary,
		"Log":    filepath.Join(home, "Library", "Logs", "spectate-agent.log"),
	}); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	return run("launchctl", "load", "-w", path)
}

// Uninstall unloads and removes the launchd agent.
func Uninstall() error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	_ = run("launchctl", "unload", "-w", path)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
