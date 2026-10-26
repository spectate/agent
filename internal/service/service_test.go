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
		"Restart=always", "RestartPreventExitStatus=78\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q", want)
		}
	}
}

func TestUpdateUnits(t *testing.T) {
	svc, timer, err := UpdateUnits(Options{Binary: "/usr/local/bin/spectate-agent"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Type=oneshot", "ExecStart=/usr/local/bin/spectate-agent update --if-newer"} {
		if !strings.Contains(svc, want) {
			t.Errorf("update service missing %q", want)
		}
	}
	if strings.Contains(svc, "User=") {
		t.Error("the updater must run as root (it replaces the binary)")
	}
	for _, want := range []string{"OnCalendar=daily", "RandomizedDelaySec=6h", "Persistent=true", "WantedBy=timers.target"} {
		if !strings.Contains(timer, want) {
			t.Errorf("timer missing %q", want)
		}
	}
}

func TestPackagesUnitsRunUnprivilegedWithRoomForThePackageManager(t *testing.T) {
	svc, timer, err := PackagesUnits(Options{Binary: "/usr/local/bin/spectate-agent"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Type=oneshot", "ExecStart=/usr/local/bin/spectate-agent check-updates", "User=spectate-agent",
		"StateDirectory=spectate-agent\n", "CacheDirectory=spectate-agent-packages\n", "MemoryMax=512M",
		"ProtectSystem=strict", "NoNewPrivileges=yes", "CapabilityBoundingSet=\n",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("package check unit missing %q", want)
		}
	}
	for _, want := range []string{"OnUnitActiveSec=1h", "RandomizedDelaySec=15min", "WantedBy=timers.target"} {
		if !strings.Contains(timer, want) {
			t.Errorf("timer missing %q", want)
		}
	}
}
