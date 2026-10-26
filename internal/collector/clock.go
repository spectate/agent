package collector

import (
	"context"
	"strings"
	"time"
)

// Clock reports whether the system clock is synchronised (1) or not (0), as
// systemd-timedated sees it: NTPSynchronized covers systemd-timesyncd,
// chrony and ntpd alike. The agent cannot ask the kernel itself (adjtimex is
// blocked by the unit's ProtectClock). Hosts without systemd report nothing;
// how far off the clock is, the server measures on every request.
type Clock struct {
	Run    Runner // default Exec
	RunDir string // default /run (HOST_RUN)
}

func (*Clock) Name() string { return "clock" }

func (*Clock) IntervalSeconds() int { return 60 }

func (c *Clock) Collect(ctx context.Context, _ time.Time, emit Emit) error {
	if !systemdBooted(orDefault(c.RunDir, runRoot())) {
		return nil
	}
	run := c.Run
	if run == nil {
		run = Exec
	}
	out, err := run(ctx, "timedatectl", "show", "--property=NTPSynchronized", "--value")
	if err != nil {
		return nil // timedated unavailable (minimal containers)
	}
	switch strings.TrimSpace(string(out)) {
	case "yes":
		emit("system.clock.synced", nil, 1)
	case "no":
		emit("system.clock.synced", nil, 0)
	}
	return nil
}
