// Package collector gathers system metrics. Metric keys and units match the v1
// agent exactly (memory/swap in MB, disk and network in KB, CPU in percent) so
// the server stores v1 and v2 data identically:
//
//	system.cpu.{user,system,interrupt,iowait,idle,stolen,guest}
//	system.memory.{total,free,used,available,available_percent}
//	system.swap.{total,free,used,in,out,free_percent}
//	system.load.{1,5,15}
//	system.uptime
//	system.disk_usage.{total,free,used,used_percent,inodes_*}  labels: mountpoint, device, fstype
//	system.net.{bytes,packets}_{sent,recv}, {err,drop,fifo}_{in,out}  label: interface
//
// Network values are amounts per 60 seconds regardless of the collection
// interval (v1 collected once a minute), so dashboards and thresholds keep
// their meaning at 15s resolution.
//
// Collectors added in v2 (not part of v1) report rates per second:
//
//	system.io.{read_bytes,write_bytes} (B/s), {reads,writes} (ops/s), busy_percent, await_ms  label: device
//	system.pressure.{cpu,memory,io}.some, {memory,io}.full (% of time stalled)
//	system.oom.kills (processes killed for lack of memory in the round)
//	system.services.failed (count), system.service.failed = 1  label: unit
//	system.updates.{pending,security,checked_age}, system.reboot.required (0/1)  label: packages
//	system.net.tcp.{established,time_wait,orphaned,inuse}
//	system.files.{used,max}
//	system.clock.synced (0/1)
//
// A collector that has nothing to report on a host (no PSI before Linux 4.20,
// no systemd, macOS) emits nothing: absent, not zero.
package collector

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/spectate/agent/v2/internal/config"
	"github.com/spectate/agent/v2/internal/metrics"
)

const (
	kb = 1024.0
	mb = 1024.0 * 1024.0
)

// Emit records one sample of the current collection round.
type Emit func(metric string, labels []metrics.Label, value float64)

// Collector produces samples for one subsystem.
type Collector interface {
	Name() string
	Collect(ctx context.Context, now time.Time, emit Emit) error
}

// Configurable collectors receive the server-driven config.
type Configurable interface {
	Configure(cfg config.Remote)
}

// Periodic collectors run less often than every round: at most once per
// IntervalSeconds (in units of the collection interval's second).
type Periodic interface {
	IntervalSeconds() int
}

// System returns the system collectors (the "agent" health collector is
// owned by the runner). stateDir is where the updates helper writes its result.
func System(stateDir string) []Collector {
	return []Collector{
		&CPU{}, &Memory{}, &Load{}, &Disk{}, &Net{}, &Uptime{},
		&DiskIO{}, &Pressure{}, &Services{}, &Updates{StateDir: stateDir}, &Sockets{}, &Clock{},
	}
}

// round rounds to 3 decimals so values fit the compact decimal wire encoding;
// that is far below what any dashboard or threshold distinguishes.
func round(v float64) float64 { return math.Round(v*1000) / 1000 }

// match reports whether name equals an entry or starts with a `prefix*` entry.
func match(patterns []string, name string, lower bool) bool {
	if lower {
		name = strings.ToLower(name)
	}
	for _, p := range patterns {
		if lower {
			p = strings.ToLower(p)
		}
		if p == name || (strings.HasSuffix(p, "*") && strings.HasPrefix(name, p[:len(p)-1])) {
			return true
		}
	}
	return false
}
