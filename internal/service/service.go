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
	// Packages also installs the hourly package update check (Linux, where
	// apt, dnf or yum is installed).
	Packages bool
}

const (
	// UnitPath is where the systemd unit is written.
	UnitPath = "/etc/systemd/system/" + Name + ".service"
	// UpdateName is the oneshot unit that runs `update --if-newer`.
	UpdateName = Name + "-update"
	// UpdateServicePath and UpdateTimerPath hold the auto-update units.
	UpdateServicePath = "/etc/systemd/system/" + UpdateName + ".service"
	UpdateTimerPath   = "/etc/systemd/system/" + UpdateName + ".timer"
	// PackagesName is the oneshot unit that runs `check-updates` (pending
	// package updates), separate from the agent because package managers need
	// far more memory than the agent's limit.
	PackagesName        = Name + "-packages"
	PackagesServicePath = "/etc/systemd/system/" + PackagesName + ".service"
	PackagesTimerPath   = "/etc/systemd/system/" + PackagesName + ".timer"
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

// The package check runs as the agent's user with the same hardening, but
// with room for apt/dnf (MemoryMax) and a cache of its own for dnf metadata.
// It never changes the system: apt reads the lists on disk, and dnf's cache
// is CacheDirectory. Its result goes to the shared state directory.
var packagesServiceTemplate = template.Must(template.New("packages-service").Parse(`[Unit]
Description=Check for pending package updates (Spectate host monitoring agent)
Documentation=https://github.com/spectate/agent#pending-updates
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User={{.User}}
Group={{.User}}
ExecStart={{.Binary}} check-updates
EnvironmentFile=-{{.EnvFile}}
StateDirectory=spectate-agent
StateDirectoryMode=0700
CacheDirectory={{.CacheDir}}
CacheDirectoryMode=0700
TimeoutStartSec=15min
Nice=19
IOSchedulingClass=idle

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
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
CapabilityBoundingSet=
SystemCallArchitectures=native
MemoryMax=512M
`))

const packagesTimer = `[Unit]
Description=Hourly package update check for the Spectate host monitoring agent

[Timer]
OnBootSec=5min
OnUnitActiveSec=1h
RandomizedDelaySec=15min

[Install]
WantedBy=timers.target
`

func (o Options) withDefaults() Options {
	if o.User == "" {
		o.User = Name
	}
	if o.EnvFile == "" {
		o.EnvFile = "/etc/spectate-agent/agent.env"
	}
	return o
}

// PackagesUnits renders the package update check service and timer.
func PackagesUnits(o Options) (service, timer string, err error) {
	var b bytes.Buffer
	if err := packagesServiceTemplate.Execute(&b, struct {
		Options
		CacheDir string
	}{o.withDefaults(), PackagesName}); err != nil {
		return "", "", err
	}
	return b.String(), packagesTimer, nil
}

// Unit renders the systemd unit.
func Unit(o Options) (string, error) {
	o = o.withDefaults()
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
