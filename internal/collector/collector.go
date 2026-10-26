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

// System returns the system collectors (the "agent" health collector is
// owned by the runner).
func System() []Collector {
	return []Collector{&CPU{}, &Memory{}, &Load{}, &Disk{}, &Net{}, &Uptime{}}
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
