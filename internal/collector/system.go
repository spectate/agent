package collector

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
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

// DefaultUsageTimeout bounds one mountpoint's statfs call.
const DefaultUsageTimeout = 2 * time.Second

// Disk reports usage per mounted filesystem.
//
// statfs on a hung mount (dead NFS server, stuck FUSE daemon) blocks in the
// kernel and cannot be interrupted, so each mountpoint is queried in its own
// goroutine with its own deadline. A mountpoint whose call is still hanging
// is skipped (not queried again) until that call returns, so one dead mount
// neither hides the other disks nor piles up goroutines.
type Disk struct {
	// UsageTimeout bounds each mountpoint's usage call (default 2s).
	UsageTimeout time.Duration

	mu                 sync.RWMutex
	includeMountpoints []string
	excludeMountpoints []string
	excludeDevices     []string
	excludeFilesystems []string

	// Test seams (default gopsutil).
	partitions func(ctx context.Context, all bool) ([]disk.PartitionStat, error)
	usage      func(ctx context.Context, path string) (*disk.UsageStat, error)

	hungMu sync.Mutex
	hung   map[string]time.Time // mountpoint -> start of the call still running
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
	partitions := d.partitions
	if partitions == nil {
		partitions = disk.PartitionsWithContext
	}
	parts, err := partitions(ctx, false)
	if err != nil && len(parts) == 0 {
		return err
	}
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		if seen[p.Mountpoint] || d.Excluded(p) {
			continue
		}
		seen[p.Mountpoint] = true
		if ctx.Err() != nil {
			return ctx.Err()
		}

		u, err := d.usageOf(ctx, p.Mountpoint)
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

// Hung returns the mountpoints whose usage call has not returned yet.
func (d *Disk) Hung() []string {
	d.hungMu.Lock()
	defer d.hungMu.Unlock()
	out := make([]string, 0, len(d.hung))
	for mp := range d.hung {
		out = append(out, mp)
	}
	sort.Strings(out)
	return out
}

var errMountHung = errors.New("usage call still hanging")

// usageOf runs one usage call with its own deadline. On timeout the call is
// left running (it cannot be cancelled) and the mountpoint is skipped until
// it returns.
func (d *Disk) usageOf(ctx context.Context, mountpoint string) (*disk.UsageStat, error) {
	d.hungMu.Lock()
	if _, ok := d.hung[mountpoint]; ok {
		d.hungMu.Unlock()
		return nil, errMountHung
	}
	if d.hung == nil {
		d.hung = make(map[string]time.Time)
	}
	start := time.Now()
	d.hung[mountpoint] = start // cleared below unless the call hangs
	d.hungMu.Unlock()

	usage := d.usage
	if usage == nil {
		usage = disk.UsageWithContext
	}
	timeout := d.UsageTimeout
	if timeout <= 0 {
		timeout = DefaultUsageTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		u   *disk.UsageStat
		err error
	}
	done := make(chan result, 1)
	var timedOut atomic.Bool
	go func() {
		u, err := usage(cctx, mountpoint)
		d.hungMu.Lock()
		delete(d.hung, mountpoint)
		d.hungMu.Unlock()
		if timedOut.Load() {
			slog.Info("disk usage call returned, collecting the mountpoint again",
				"mountpoint", mountpoint, "after", time.Since(start).Round(time.Millisecond))
		}
		done <- result{u, err}
	}()

	select {
	case r := <-done:
		return r.u, r.err
	case <-cctx.Done():
		timedOut.Store(true)
		select {
		case r := <-done: // returned at the deadline
			return r.u, r.err
		default:
		}
		slog.Warn("disk usage call hangs (stuck mount?); skipping the mountpoint until it returns",
			"mountpoint", mountpoint, "timeout", timeout)
		return nil, errMountHung
	}
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
