// Package service installs the agent as a system service: a hardened systemd
// unit running as an unprivileged user on Linux, a launchd agent on macOS.
package service

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"text/template"
)

// Name is the service/unit name.
const Name = "spectate-agent"

// ExitHostDeleted is the exit status of `run` when the host was deleted in
// Spectate. The unit does not restart on it (RestartPreventExitStatus).
const ExitHostDeleted = 78 // EX_CONFIG

// Options for Install.
type Options struct {
	Binary     string // absolute path of the agent binary
	User       string // service user (Linux)
	EnvFile    string // optional EnvironmentFile (Linux)
	AutoUpdate bool   // also install the daily update timer (Linux)
}

const (
	// UnitPath is where the systemd unit is written.
	UnitPath = "/etc/systemd/system/" + Name + ".service"
	// UpdateName is the oneshot unit that runs `update --if-newer`.
	UpdateName = Name + "-update"
	// UpdateServicePath and UpdateTimerPath hold the auto-update units.
	UpdateServicePath = "/etc/systemd/system/" + UpdateName + ".service"
	UpdateTimerPath   = "/etc/systemd/system/" + UpdateName + ".timer"
)

var unitTemplate = template.Must(template.New("unit").Parse(`[Unit]
Description=Spectate host monitoring agent
Documentation=https://docs.spectate.net/host-agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User={{.User}}
Group={{.User}}
ExecStart={{.Binary}} run
EnvironmentFile=-{{.EnvFile}}
Restart=always
RestartSec=5
# Exit status {{.ExitHostDeleted}}: the host was deleted in Spectate; restarting cannot help.
RestartPreventExitStatus={{.ExitHostDeleted}}
TimeoutStopSec=15
KillSignal=SIGTERM

# State (host token, server config, spool) lives in /var/lib/spectate-agent.
StateDirectory=spectate-agent
StateDirectoryMode=0700

# Hardening: the agent only reads /proc, /sys and filesystem stats.
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
CapabilityBoundingSet=
SystemCallArchitectures=native
MemoryMax=64M
Nice=10

[Install]
WantedBy=multi-user.target
`))

// The agent runs unprivileged on a read-only filesystem and cannot replace
// itself; this root oneshot does, at most daily.
var updateServiceTemplate = template.Must(template.New("update-service").Parse(`[Unit]
Description=Update the Spectate host monitoring agent
Documentation=https://github.com/spectate/agent#updating
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart={{.Binary}} update --if-newer
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
`))

const updateTimer = `[Unit]
Description=Daily update check for the Spectate host monitoring agent

[Timer]
OnCalendar=daily
RandomizedDelaySec=6h
Persistent=true

[Install]
WantedBy=timers.target
`

// Unit renders the systemd unit.
func Unit(o Options) (string, error) {
	if o.User == "" {
		o.User = Name
	}
	if o.EnvFile == "" {
		o.EnvFile = "/etc/spectate-agent/agent.env"
	}
	var b bytes.Buffer
	if err := unitTemplate.Execute(&b, struct {
		Options
		ExitHostDeleted int
	}{o, ExitHostDeleted}); err != nil {
		return "", err
	}
	return b.String(), nil
}

// UpdateUnits renders the auto-update service and timer.
func UpdateUnits(o Options) (service, timer string, err error) {
	var b bytes.Buffer
	if err := updateServiceTemplate.Execute(&b, o); err != nil {
		return "", "", err
	}
	return b.String(), updateTimer, nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w", name, args, err)
	}
	return nil
}
