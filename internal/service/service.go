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

// Options for Install.
type Options struct {
	Binary  string // absolute path of the agent binary
	User    string // service user (Linux)
	EnvFile string // optional EnvironmentFile (Linux)
}

// UnitPath is where the systemd unit is written.
const UnitPath = "/etc/systemd/system/" + Name + ".service"

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

// Unit renders the systemd unit.
func Unit(o Options) (string, error) {
	if o.User == "" {
		o.User = Name
	}
	if o.EnvFile == "" {
		o.EnvFile = "/etc/spectate-agent/agent.env"
	}
	var b bytes.Buffer
	if err := unitTemplate.Execute(&b, o); err != nil {
		return "", err
	}
	return b.String(), nil
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
