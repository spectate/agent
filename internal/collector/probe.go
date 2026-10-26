package collector

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/spectate/agent/v2/internal/config"
	"github.com/spectate/agent/v2/internal/metrics"
)

// Sample is one probed value.
type Sample struct {
	Metric string          `json:"metric"`
	Labels []metrics.Label `json:"labels,omitempty"`
	Value  float64         `json:"value"`
	Unit   string          `json:"unit,omitempty"`
}

// ProbeResult is what one collector produced in a probe.
type ProbeResult struct {
	Collector string   `json:"collector"`
	Enabled   bool     `json:"enabled"`
	Samples   []Sample `json:"samples"`
	Error     string   `json:"error,omitempty"`
	Duration  string   `json:"duration"`
}

// Probe runs every collector twice, `gap` apart (counters need two samples
// for a rate), outside the agent: nothing is shipped. Enabled tells whether
// the server's config has the collector on; disabled collectors run too, to
// show what they would report.
func Probe(ctx context.Context, collectors []Collector, remote config.Remote, gap time.Duration) []ProbeResult {
	out := make([]ProbeResult, 0, len(collectors))
	for _, c := range collectors {
		if cc, ok := c.(Configurable); ok {
			cc.Configure(remote)
		}
	}
	type acc struct {
		samples []Sample
		err     error
		took    time.Duration
	}
	results := make([]acc, len(collectors))
	for round := range 2 {
		if round == 1 {
			select {
			case <-ctx.Done():
			case <-time.After(gap):
			}
		}
		for i, c := range collectors {
			var got []Sample
			start := time.Now()
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Collect(rctx, time.Now(), func(metric string, labels []metrics.Label, value float64) {
				got = append(got, Sample{Metric: metric, Labels: labels, Value: value, Unit: Unit(metric)})
			})
			cancel()
			// Keep the second round: it has the rates
			results[i] = acc{samples: got, err: err, took: time.Since(start)}
		}
	}
	for i, c := range collectors {
		r := ProbeResult{
			Collector: c.Name(),
			Enabled:   remote.Enabled(c.Name()),
			Samples:   results[i].samples,
			Duration:  results[i].took.Round(time.Millisecond).String(),
		}
		if results[i].err != nil {
			r.Error = results[i].err.Error()
		}
		sort.SliceStable(r.Samples, func(a, b int) bool { return r.Samples[a].Metric < r.Samples[b].Metric })
		out = append(out, r)
	}
	return out
}

// Unit returns the unit of a metric, for display.
func Unit(metric string) string {
	switch {
	case strings.HasSuffix(metric, "_percent"), strings.HasPrefix(metric, "system.cpu."),
		strings.HasPrefix(metric, "system.pressure."):
		return "%"
	case strings.HasPrefix(metric, "system.memory."), strings.HasPrefix(metric, "system.swap."):
		return "MB"
	case strings.HasPrefix(metric, "system.disk_usage.inodes_"):
		return "inodes"
	case strings.HasPrefix(metric, "system.disk_usage."):
		return "KB"
	case metric == "system.net.bytes_sent" || metric == "system.net.bytes_recv":
		return "KB/60s"
	case strings.HasPrefix(metric, "system.net.tcp."):
		return "sockets"
	case strings.HasPrefix(metric, "system.net."):
		return "/60s"
	case metric == "system.io.read_bytes" || metric == "system.io.write_bytes":
		return "B/s"
	case metric == "system.io.reads" || metric == "system.io.writes":
		return "ops/s"
	case metric == "system.io.await_ms":
		return "ms"
	case metric == "system.uptime", metric == "system.updates.checked_age":
		return "s"
	case metric == "system.reboot.required", metric == "system.clock.synced", metric == "system.service.failed":
		return "0/1"
	case strings.HasPrefix(metric, "system.files."):
		return "handles"
	case metric == "system.oom.kills":
		return "kills"
	case strings.HasPrefix(metric, "system.updates."):
		return "packages"
	case metric == "system.services.failed":
		return "units"
	}
	return ""
}
