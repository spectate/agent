package collector

import (
	"bufio"
	"bytes"
	"context"
	"sort"
	"strings"
	"time"

	"github.com/spectate/agent/v2/internal/metrics"
)

// maxFailedUnits bounds the per-unit series of one round.
const maxFailedUnits = 50

// Services reports failed systemd services: their number, and one series per
// failed unit (label `unit`) so the server can name them. Hosts without
// systemd report nothing.
//
// `systemctl list-units` is a read-only D-Bus call that works unprivileged
// and under the agent unit's hardening. It runs once a minute.
type Services struct {
	Run    Runner // default Exec
	RunDir string // default /run (HOST_RUN)
}

func (*Services) Name() string { return "services" }

func (*Services) IntervalSeconds() int { return 60 }

func (s *Services) Collect(ctx context.Context, _ time.Time, emit Emit) error {
	if !systemdBooted(orDefault(s.RunDir, runRoot())) {
		return nil
	}
	run := s.Run
	if run == nil {
		run = Exec
	}
	out, err := run(ctx, "systemctl", "list-units", "--type=service", "--state=failed", "--all", "--plain", "--no-legend", "--no-pager")
	if err != nil {
		return err
	}
	units := FailedUnits(out)
	emit("system.services.failed", nil, float64(len(units)))
	for i, unit := range units {
		if i == maxFailedUnits {
			break
		}
		emit("system.service.failed", []metrics.Label{{Key: "unit", Value: unit}}, 1)
	}
	return nil
}

// FailedUnits parses `systemctl list-units --plain --no-legend` output: the
// first column of every line is the unit name.
func FailedUnits(out []byte) []string {
	var units []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		name := strings.TrimLeft(f[0], "●*×") // markers some versions print even with --plain
		if name == "" && len(f) > 1 {
			name = f[1]
		}
		if strings.Contains(name, ".") {
			units = append(units, name)
		}
	}
	sort.Strings(units)
	return units
}
