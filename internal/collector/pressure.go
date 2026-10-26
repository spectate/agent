package collector

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Pressure reports pressure stall information (Linux 4.20+) and OOM kills
// (Linux 4.13+).
//
// PSI: the share of time in the round that some (or all, "full") runnable
// tasks were stalled waiting for CPU, memory or I/O, from each file's
// cumulative `total` (µs), so the value covers the whole round rather than the
// kernel's 10s average. CPU "full" is not reported: at the system level it is
// always zero.
//
// OOM kills: processes the kernel killed for lack of memory during the round
// (/proc/vmstat oom_kill).
type Pressure struct {
	Proc string // procfs root (default /proc or HOST_PROC)

	last     map[string]float64 // "memory.some" -> total µs
	lastTime time.Time
	lastOOM  *float64
}

func (*Pressure) Name() string { return "pressure" }

var pressureResources = [...]struct {
	resource string
	full     bool
}{{"cpu", false}, {"memory", true}, {"io", true}}

func (p *Pressure) Collect(_ context.Context, now time.Time, emit Emit) error {
	root := orDefault(p.Proc, procRoot())

	totals := make(map[string]float64, 5)
	for _, r := range pressureResources {
		data, err := os.ReadFile(filepath.Join(root, "pressure", r.resource))
		if err != nil {
			continue // no PSI (older kernel, psi=0, macOS)
		}
		for kind, total := range parsePressure(data) {
			if kind == "full" && !r.full {
				continue
			}
			totals[r.resource+"."+kind] = total
		}
	}
	prev, prevTime := p.last, p.lastTime
	p.last, p.lastTime = totals, now
	if elapsed := now.Sub(prevTime); prev != nil && elapsed > 0 {
		us := float64(elapsed.Microseconds())
		for key, total := range totals {
			before, ok := prev[key]
			if !ok || total < before {
				continue
			}
			emit("system.pressure."+key, nil, round(max(0, min(100, (total-before)/us*100))))
		}
	}

	if data, err := os.ReadFile(filepath.Join(root, "vmstat")); err == nil {
		if kills, ok := fields(data)["oom_kill"]; ok {
			if p.lastOOM != nil {
				emit("system.oom.kills", nil, max(0, kills-*p.lastOOM))
			}
			p.lastOOM = &kills
		}
	}
	return nil
}

// parsePressure reads the `total=` of the "some" and "full" lines of a
// /proc/pressure file.
func parsePressure(data []byte) map[string]float64 {
	out := make(map[string]float64, 2)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "total="); ok {
				if n, err := strconv.ParseFloat(v, 64); err == nil {
					out[f[0]] = n
				}
			}
		}
	}
	return out
}
