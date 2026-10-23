package service

import (
	"strings"
	"testing"
)

func TestUnitRunsUnprivilegedAndHardened(t *testing.T) {
	unit, err := Unit(Options{Binary: "/usr/local/bin/spectate-agent"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"User=spectate-agent", "ExecStart=/usr/local/bin/spectate-agent run", "StateDirectory=spectate-agent",
		"EnvironmentFile=-/etc/spectate-agent/agent.env", "NoNewPrivileges=yes", "ProtectSystem=strict", "CapabilityBoundingSet=\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q", want)
		}
	}
}
