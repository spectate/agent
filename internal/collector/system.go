package collector

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/spectate/agent/v2/internal/config"
	"github.com/spectate/agent/v2/internal/metrics"
)

// CPU reports the share of CPU time per state since the previous round.
type CPU struct {
	last *cpu.TimesStat
}

func (*CPU) Name() string { return "cpu" }

func (c *CPU) Collect(ctx context.Context, _ time.Time, emit Emit) error {
	times, err := cpu.TimesWithContext(ctx, false)
	if err != nil {
		return err
	}
	if len(times) == 0 {
		return errors.New("no cpu times")
	}
	cur := times[0]
	prev := c.last
	c.last = &cur
	if prev == nil {
		return nil
	}
	CPUPercentages(*prev, cur, emit)
	return nil
}

// CPUPercentages emits the v1 agent's CPU breakdown between two samples.
func CPUPercentages(prev, cur cpu.TimesStat, emit Emit) {
	total := func(t cpu.TimesStat) float64 {
		return t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal
	}
	elapsed := total(cur) - total(prev)
	if elapsed <= 0 {
		return
	}
	pct := func(d float64) float64 { return round(max(0, min(100, d/elapsed*100))) }

	emit("system.cpu.user", nil, pct((cur.User+cur.Nice)-(prev.User+prev.Nice)))
	emit("system.cpu.system", nil, pct((cur.System+cur.Irq+cur.Softirq)-(prev.System+prev.Irq+prev.Softirq)))
	emit("system.cpu.interrupt", nil, pct((cur.Irq+cur.Softirq)-(prev.Irq+prev.Softirq)))
	emit("system.cpu.iowait", nil, pct(cur.Iowait-prev.Iowait))
	emit("system.cpu.idle", nil, pct(cur.Idle-prev.Idle))
	emit("system.cpu.stolen", nil, pct(cur.Steal-prev.Steal))
	emit("system.cpu.guest", nil, pct(cur.Guest-prev.Guest))
}

// Memory reports RAM and swap.
type Memory struct{}

func (Memory) Name() string { return "memory" }

func (Memory) Collect(ctx context.Context, _ time.Time, emit Emit) error {
	v, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return err
	}
	emit("system.memory.total", nil, round(float64(v.Total)/mb))
	emit("system.memory.free", nil, round(float64(v.Free)/mb))
	emit("system.memory.used", nil, round(float64(v.Used)/mb))
	emit("system.memory.available", nil, round(float64(v.Available)/mb))
	emit("system.memory.available_percent", nil, round(100-v.UsedPercent))

	s, err := mem.SwapMemoryWithContext(ctx)
	if err != nil {
		return nil // no swap information (e.g. some containers)
	}
	emit("system.swap.total", nil, round(float64(s.Total)/mb))
	emit("system.swap.free", nil, round(float64(s.Free)/mb))
	emit("system.swap.used", nil, round(float64(s.Used)/mb))
	emit("system.swap.in", nil, round(float64(s.Sin)/mb))
	emit("system.swap.out", nil, round(float64(s.Sout)/mb))
	freePct := 100.0
	if s.Total > 0 {
		freePct = 100 - s.UsedPercent
	}
	emit("system.swap.free_percent", nil, round(freePct))
	return nil
}

// Load reports load averages.
type Load struct{}

func (Load) Name() string { return "load" }

func (Load) Collect(ctx context.Context, _ time.Time, emit Emit) error {
	l, err := load.AvgWithContext(ctx)
	if err != nil {
		return err
	}
	emit("system.load.1", nil, round(l.Load1))
	emit("system.load.5", nil, round(l.Load5))
	emit("system.load.15", nil, round(l.Load15))
	return nil
}

// Uptime reports seconds since boot.
type Uptime struct{}

func (Uptime) Name() string { return "uptime" }

func (Uptime) Collect(ctx context.Context, _ time.Time, emit Emit) error {
	u, err := host.UptimeWithContext(ctx)
	if err != nil {
		return err
	}
	emit("system.uptime", nil, float64(u))
	return nil
}

// Disk reports usage per mounted filesystem.
type Disk struct {
	mu                 sync.RWMutex
	includeMountpoints []string
	excludeMountpoints []string
	excludeDevices     []string
	excludeFilesystems []string
}

func (*Disk) Name() string { return "disk" }

func (d *Disk) Configure(cfg config.Remote) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.includeMountpoints = cfg.Disk.IncludeMountpoints
	d.excludeMountpoints = cfg.Disk.ExcludeMountpoints
	d.excludeDevices = cfg.Disk.ExcludeDevices
	d.excludeFilesystems = cfg.Disk.ExcludeFilesystems
}

// Excluded applies the include/exclude lists (case-insensitive, `prefix*` wildcards).
func (d *Disk) Excluded(p disk.PartitionStat) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(d.includeMountpoints) > 0 && !match(d.includeMountpoints, p.Mountpoint, true) {
		return true
	}
	return match(d.excludeFilesystems, p.Fstype, true) ||
		match(d.excludeDevices, p.Device, true) ||
		match(d.excludeMountpoints, p.Mountpoint, true)
}

func (d *Disk) Collect(ctx context.Context, _ time.Time, emit Emit) error {
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil && len(parts) == 0 {
		return err
	}
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		if seen[p.Mountpoint] || d.Excluded(p) {
			continue
		}
		seen[p.Mountpoint] = true

		u, err := disk.UsageWithContext(ctx, p.Mountpoint)
		if err != nil || u.Total == 0 {
			continue
		}
		labels := []metrics.Label{{Key: "mountpoint", Value: p.Mountpoint}, {Key: "device", Value: p.Device}, {Key: "fstype", Value: p.Fstype}}
		emit("system.disk_usage.total", labels, round(float64(u.Total)/kb))
		emit("system.disk_usage.free", labels, round(float64(u.Free)/kb))
		emit("system.disk_usage.used", labels, round(float64(u.Used)/kb))
		emit("system.disk_usage.used_percent", labels, round(u.UsedPercent))
		emit("system.disk_usage.inodes_total", labels, float64(u.InodesTotal))
		emit("system.disk_usage.inodes_used", labels, float64(u.InodesUsed))
		emit("system.disk_usage.inodes_free", labels, float64(u.InodesFree))
		emit("system.disk_usage.inodes_used_percent", labels, round(u.InodesUsedPercent))
	}
	return nil
}

// Net reports per-interface traffic since the previous round, normalised to
// amounts per 60 seconds.
type Net struct {
	mu       sync.RWMutex
	include  []string
	exclude  []string
	last     map[string]net.IOCountersStat
	lastTime time.Time
}

func (*Net) Name() string { return "net" }

func (n *Net) Configure(cfg config.Remote) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.include = cfg.Net.IncludeInterfaces
	n.exclude = cfg.Net.ExcludeInterfaces
}

// Excluded applies the v1 semantics: include (exact) beats exclude (wildcards).
func (n *Net) Excluded(iface string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if match(n.include, iface, false) {
		return false
	}
	return match(n.exclude, iface, false)
}

func (n *Net) Collect(ctx context.Context, now time.Time, emit Emit) error {
	counters, err := net.IOCountersWithContext(ctx, true)
	if err != nil {
		return err
	}
	prev, prevTime := n.last, n.lastTime
	n.last = make(map[string]net.IOCountersStat, len(counters))
	n.lastTime = now
	for _, c := range counters {
		n.last[c.Name] = c
	}
	if prev == nil {
		return nil
	}
	NetDeltas(prev, counters, now.Sub(prevTime), n.Excluded, emit)
	return nil
}

// NetDeltas emits per-interface deltas scaled to 60 seconds. Interfaces whose
// counters went backwards (reset, reattach) are skipped for this round.
func NetDeltas(prev map[string]net.IOCountersStat, cur []net.IOCountersStat, elapsed time.Duration, excluded func(string) bool, emit Emit) {
	if elapsed <= 0 {
		return
	}
	scale := 60 / elapsed.Seconds()
	for _, c := range cur {
		p, ok := prev[c.Name]
		if !ok || excluded(c.Name) {
			continue
		}
		pairs := [...]struct {
			metric    string
			cur, prev uint64
			bytes     bool
		}{
			{"system.net.bytes_sent", c.BytesSent, p.BytesSent, true},
			{"system.net.bytes_recv", c.BytesRecv, p.BytesRecv, true},
			{"system.net.packets_sent", c.PacketsSent, p.PacketsSent, false},
			{"system.net.packets_recv", c.PacketsRecv, p.PacketsRecv, false},
			{"system.net.err_in", c.Errin, p.Errin, false},
			{"system.net.err_out", c.Errout, p.Errout, false},
			{"system.net.drop_in", c.Dropin, p.Dropin, false},
			{"system.net.drop_out", c.Dropout, p.Dropout, false},
			{"system.net.fifo_in", c.Fifoin, p.Fifoin, false},
			{"system.net.fifo_out", c.Fifoout, p.Fifoout, false},
		}
		reset := false
		for _, pr := range pairs {
			reset = reset || pr.cur < pr.prev
		}
		if reset {
			continue
		}
		labels := []metrics.Label{{Key: "interface", Value: c.Name}}
		for _, pr := range pairs {
			delta := float64(pr.cur-pr.prev) * scale
			if pr.bytes {
				delta /= kb
			}
			emit(pr.metric, labels, round(delta))
		}
	}
}
