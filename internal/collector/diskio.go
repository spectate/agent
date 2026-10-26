package collector

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/spectate/agent/v2/internal/config"
	"github.com/spectate/agent/v2/internal/metrics"
)

// DiskIO reports I/O per physical disk since the previous round, per second:
// throughput, operations, how busy the disk was and the average time an
// operation took (queueing included).
//
// On Linux only whole disks are reported (names in /sys/block), not
// partitions, so a disk's I/O is not counted twice; stacked devices (dm-*,
// md*) are excluded by default for the same reason.
type DiskIO struct {
	mu      sync.RWMutex
	exclude []string

	// Test seams.
	counters func(ctx context.Context) (map[string]disk.IOCountersStat, error)
	sysBlock string // default <sys>/block

	last     map[string]disk.IOCountersStat
	lastTime time.Time
}

func (*DiskIO) Name() string { return "diskio" }

func (d *DiskIO) Configure(cfg config.Remote) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.exclude = cfg.DiskIO.ExcludeDevices
	if d.exclude == nil { // a server that predates the setting
		d.exclude = config.DefaultRemote().DiskIO.ExcludeDevices
	}
}

// Excluded reports whether a device is skipped (`prefix*` wildcards).
func (d *DiskIO) Excluded(device string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	exclude := d.exclude
	if exclude == nil {
		exclude = config.DefaultRemote().DiskIO.ExcludeDevices
	}
	return match(exclude, device, false)
}

func (d *DiskIO) Collect(ctx context.Context, now time.Time, emit Emit) error {
	counters := d.counters
	if counters == nil {
		counters = func(ctx context.Context) (map[string]disk.IOCountersStat, error) {
			return disk.IOCountersWithContext(ctx)
		}
	}
	cur, err := counters(ctx)
	if err != nil {
		return nil // no I/O statistics on this platform
	}
	wholeDisks := d.wholeDisks()

	prev, prevTime := d.last, d.lastTime
	d.last, d.lastTime = cur, now
	if prev == nil {
		return nil
	}
	elapsed := now.Sub(prevTime)
	if elapsed <= 0 {
		return nil
	}
	for name, c := range cur {
		if wholeDisks != nil && !wholeDisks[name] {
			continue
		}
		p, ok := prev[name]
		if !ok || d.Excluded(name) {
			continue
		}
		DiskIODeltas(name, p, c, elapsed, emit)
	}
	return nil
}

// wholeDisks returns the names in /sys/block (Linux), or nil where the
// platform reports whole disks only (macOS).
func (d *DiskIO) wholeDisks() map[string]bool {
	entries, err := os.ReadDir(orDefault(d.sysBlock, filepath.Join(sysRoot(), "block")))
	if err != nil {
		return nil
	}
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out
}

// DiskIODeltas emits one device's I/O between two samples. A counter that
// went backwards (device re-attached) skips the device for this round.
func DiskIODeltas(device string, prev, cur disk.IOCountersStat, elapsed time.Duration, emit Emit) {
	if cur.ReadCount < prev.ReadCount || cur.WriteCount < prev.WriteCount ||
		cur.ReadBytes < prev.ReadBytes || cur.WriteBytes < prev.WriteBytes ||
		cur.ReadTime < prev.ReadTime || cur.WriteTime < prev.WriteTime || cur.IoTime < prev.IoTime {
		return
	}
	secs := elapsed.Seconds()
	reads := float64(cur.ReadCount - prev.ReadCount)
	writes := float64(cur.WriteCount - prev.WriteCount)
	labels := []metrics.Label{{Key: "device", Value: device}}

	emit("system.io.read_bytes", labels, round(float64(cur.ReadBytes-prev.ReadBytes)/secs))
	emit("system.io.write_bytes", labels, round(float64(cur.WriteBytes-prev.WriteBytes)/secs))
	emit("system.io.reads", labels, round(reads/secs))
	emit("system.io.writes", labels, round(writes/secs))
	// io_ticks: milliseconds the device had I/O in flight
	emit("system.io.busy_percent", labels, round(min(100, float64(cur.IoTime-prev.IoTime)/(secs*1000)*100)))
	await := 0.0
	if ops := reads + writes; ops > 0 {
		await = float64((cur.ReadTime-prev.ReadTime)+(cur.WriteTime-prev.WriteTime)) / ops
	}
	emit("system.io.await_ms", labels, round(await))
}
