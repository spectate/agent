package encode

import (
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/spectate/agent/v2/internal/metrics"
	pb "github.com/spectate/agent/v2/internal/proto/agentv2"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

func TestDecimalEncode(t *testing.T) {
	cases := []struct {
		name   string
		values []float64
		scale  int
		ok     bool
	}{
		{"integers", []float64{0, 1, 1024, -7, 1 << 40}, 0, true},
		{"two decimals", []float64{0.75, 1.25, 0.5}, 2, true},
		{"awkward fractions", []float64{0.1, 0.2, 0.3}, 1, true},
		{"mixed", []float64{15923.445, 99.999, 1e-6}, 6, true},
		{"irrational", []float64{1.0 / 3}, 0, false},
		{"pi", []float64{math.Pi}, 0, false},
		{"negative zero", []float64{math.Copysign(0, -1)}, 0, false},
		{"beyond 2^53", []float64{1e17}, 0, false},
		{"too big for scale", []float64{1e12 + 0.1234567}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scale, ints, ok := DecimalEncode(c.values)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if scale != c.scale {
				t.Fatalf("scale = %d, want %d", scale, c.scale)
			}
			for i, v := range c.values {
				if got := float64(ints[i]) / pow10[scale]; got != v {
					t.Fatalf("value %d: %v round-trips to %v", i, v, got)
				}
			}
		})
	}
}

func TestBuildRoundTrip(t *testing.T) {
	acc := metrics.NewAccumulator(1000)
	disk := []metrics.Label{{Key: "mountpoint", Value: "/"}, {Key: "device", Value: "/dev/sda1"}, {Key: "fstype", Value: "ext4"}}
	base := int64(1_792_490_400_250)
	for i := range int64(4) {
		ts := base + i*15_000
		acc.Add("system.cpu.idle", nil, ts, 70.125-float64(i))
		acc.Add("system.disk_usage.used_percent", disk, ts, 41.237)
		acc.Add("system.disk_usage.total", disk, ts, 102_687_744)
		acc.Add("custom.ratio", []metrics.Label{{Key: "zone", Value: "eu"}}, ts+int64(i), 1.0/3+float64(i))
	}
	acc.Add("late", nil, base-5_000, -1.5) // earlier than everything: becomes the base

	series := acc.Drain()
	req := Build(Envelope{AgentVersion: "2.0.0", AgentInstance: "i", BatchSeq: 3}, series)

	if req.BaseTimestampMs != base-5_000 {
		t.Fatalf("base = %d", req.BaseTimestampMs)
	}
	if req.Strings[0] != "" {
		t.Fatal("string 0 must be empty")
	}
	// Labels are shared through the string table: "/" appears once.
	count := 0
	for _, s := range req.Strings {
		if s == "/" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf(`"/" appears %d times in the string table`, count)
	}
	if req.Series[0].Scale != 3 || len(req.Series[0].Values) != 0 {
		t.Fatalf("cpu.idle should use decimal encoding at scale 3, got scale %d", req.Series[0].Scale)
	}
	if len(req.Series[3].Values) != 4 {
		t.Fatal("custom.ratio should fall back to doubles")
	}
	if got := req.Series[1].TimestampDeltas; !reflect.DeepEqual(got, []int64{5_000, 15_000, 15_000, 15_000}) {
		t.Fatalf("timestamp deltas = %v", got)
	}

	body, err := Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Unmarshal(body)
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := Expand(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expanded, series) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", expanded, series)
	}
}

func TestHostInfoHashIsStable(t *testing.T) {
	a := &pb.HostInfo{Info: []*pb.Label{{Key: "hostname", Value: "web-01"}}}
	b := &pb.HostInfo{Info: []*pb.Label{{Key: "hostname", Value: "web-01"}}}
	c := &pb.HostInfo{Info: []*pb.Label{{Key: "hostname", Value: "web-02"}}}
	if HostInfoHash(a) != HostInfoHash(b) || HostInfoHash(a) == HostInfoHash(c) || len(HostInfoHash(a)) != 64 {
		t.Fatal("host info hash must be a stable sha256 hex digest")
	}
}

// TestGoldenBatch writes the fixture the Laravel suite decodes
// (tests/Feature/HostAgentV2Test.php: "accepts a batch produced by the Go agent").
// Regenerate with: go test ./internal/encode -run Golden -update
func TestGoldenBatch(t *testing.T) {
	acc := metrics.NewAccumulator(1000)
	base := int64(1_792_490_400_000) // 2026-10-20 10:00:00 UTC
	disk := []metrics.Label{{Key: "mountpoint", Value: "/"}, {Key: "device", Value: "/dev/nvme0n1p1"}, {Key: "fstype", Value: "ext4"}}
	idle := []float64{71.234, 68.5, 65.125, 60.0}
	for i, v := range idle {
		ts := base + int64(i)*15_000
		acc.Add("system.cpu.idle", nil, ts, v)
		acc.Add("system.disk_usage.used_percent", disk, ts, 41.237)
		acc.Add("system.net.bytes_recv", []metrics.Label{{Key: "interface", Value: "eth0"}}, ts, 3391.5+float64(i))
	}
	info := &pb.HostInfo{Info: []*pb.Label{{Key: "hostname", Value: "golden-01"}, {Key: "os", Value: "linux"}}}
	req := Build(Envelope{
		AgentVersion:  "2.0.0-golden",
		AgentInstance: "golden",
		BatchSeq:      1,
		CreatedAtMs:   base + 60_000,
		HostInfoHash:  HostInfoHash(info),
		HostInfo:      info,
	}, acc.Drain())

	body, err := Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := json.MarshalIndent(map[string]any{
		"cpu_idle":    idle,
		"disk_labels": []string{"/", "/dev/nvme0n1p1", "ext4"},
		"hostname":    "golden-01",
	}, "", "  ")

	dir := filepath.Join("testdata")
	if *update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "golden-batch.pb.gz"), body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "golden-batch.json"), append(expected, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	stored, err := os.ReadFile(filepath.Join(dir, "golden-batch.pb.gz"))
	if err != nil {
		t.Fatalf("missing golden file (run with -update): %v", err)
	}
	got, err := Unmarshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Expand(got)
	b, _ := Expand(req)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("golden batch is stale; run with -update")
	}
}

// typicalMinute mirrors typicalMinute() in laravel/tests/Feature/HostAgentV2Test.php:
// 4 rounds at 15s, 7 cpu + 5 memory + 6 swap + 3 load + uptime series, 2 disks
// x 8, 2 interfaces x 10 (58 series, 232 samples).
func typicalMinute() []*metrics.Series {
	acc := metrics.NewAccumulator(10_000)
	start := int64(1_792_490_400_123)
	for round := range 4 {
		ts := start + int64(round)*15_000
		r := float64(round)
		add := func(m string, v float64, l ...metrics.Label) { acc.Add(m, l, ts, math.Round(v*1000)/1000) }
		for i, k := range []string{"user", "system", "interrupt", "iowait", "stolen", "guest"} {
			add("system.cpu."+k, []float64{18.112, 6.03, 0.41, 1.207, 0, 0}[i]+r*0.731)
		}
		add("system.cpu.idle", 71.234-r*3.5)
		add("system.memory.total", 15923.445)
		add("system.memory.free", 1203.113-r*12.5)
		add("system.memory.used", 9871.07+r*11.25)
		add("system.memory.available", 5642.875-r*9.125)
		add("system.memory.available_percent", 35.437-r*0.057)
		for i, k := range []string{"total", "free", "used", "in", "out", "free_percent"} {
			add("system.swap."+k, []float64{2048, 1536, 512, 10.25, 31.5, 75}[i])
		}
		add("system.load.1", 0.82+r*0.05)
		add("system.load.5", 0.71)
		add("system.load.15", 0.65)
		add("system.uptime", 1_234_567+r*15)
		for _, d := range [][3]string{{"/", "/dev/nvme0n1p1", "ext4"}, {"/var/lib/docker", "/dev/nvme1n1", "xfs"}} {
			l := []metrics.Label{{Key: "mountpoint", Value: d[0]}, {Key: "device", Value: d[1]}, {Key: "fstype", Value: d[2]}}
			for i, k := range []string{"total", "free", "used", "used_percent", "inodes_total", "inodes_used", "inodes_free", "inodes_used_percent"} {
				add("system.disk_usage."+k, []float64{102687744, 60342011, 42345733 + r*4, 41.237, 6553600, 412331 + r, 6141269 - r, 6.292}[i], l...)
			}
		}
		for _, iface := range []string{"eth0", "eth1"} {
			l := metrics.Label{Key: "interface", Value: iface}
			for i, k := range []string{"bytes_sent", "bytes_recv", "packets_sent", "packets_recv", "err_in", "err_out", "drop_in", "drop_out", "fifo_in", "fifo_out"} {
				add("system.net."+k, []float64{812.375 + r*17, 3391.5 + r*17, 1204 + r*17, 2877 + r*17, 0, 0, 0, 0, 0, 0}[i], l)
			}
		}
		// v2.1 collectors: disk I/O, pressure, OOM kills, sockets, file handles
		for _, dev := range []string{"nvme0n1", "nvme1n1"} {
			l := metrics.Label{Key: "device", Value: dev}
			for i, k := range []string{"read_bytes", "write_bytes", "reads", "writes", "busy_percent", "await_ms"} {
				add("system.io."+k, []float64{48_311.467 + r*1000, 1_310_720 + r*2048, 3.267 + r, 41.533 + r, 2.4 + r*0.1, 0.415}[i], l)
			}
		}
		for i, k := range []string{"cpu.some", "memory.some", "memory.full", "io.some", "io.full"} {
			add("system.pressure."+k, []float64{1.234 + r*0.1, 0, 0, 0.412, 0.103}[i])
		}
		add("system.oom.kills", 0)
		for i, k := range []string{"established", "time_wait", "orphaned", "inuse"} {
			add("system.net.tcp."+k, []float64{142 + r, 37, 0, 181 + r}[i])
		}
		add("system.files.used", 4128+r*3)
		add("system.files.max", 9_223_372_036_854_775_807)
		if round == 0 { // once a minute
			add("system.services.failed", 0)
			add("system.updates.pending", 12)
			add("system.updates.security", 3)
			add("system.updates.checked_age", 2400)
			add("system.reboot.required", 0)
			add("system.clock.synced", 1)
		}
	}
	return acc.Drain()
}

func TestTypicalMinuteSize(t *testing.T) {
	series := typicalMinute()
	req := Build(Envelope{AgentVersion: "2.0.0", AgentInstance: "0123456789abcdef", BatchSeq: 1, HostInfoHash: HostInfoHash(&pb.HostInfo{})}, series)
	body, err := Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := Unmarshal(body)
	samples := 0
	for _, s := range series {
		samples += len(s.Values)
	}
	t.Logf("typical minute: %d series, %d samples, %d bytes gzip (%.1f bytes/sample)", len(series), samples, len(body), float64(len(body))/float64(samples))
	if raw == nil || len(body) > 1500 {
		t.Fatalf("typical minute is %d bytes gzip, expected well under 1.5 KB", len(body))
	}
}

func BenchmarkEncodeTypicalMinute(b *testing.B) {
	series := typicalMinute()
	env := Envelope{AgentVersion: "2.0.0", AgentInstance: "0123456789abcdef", BatchSeq: 1}
	var n int
	b.ReportAllocs()
	for b.Loop() {
		body, err := Marshal(Build(env, series))
		if err != nil {
			b.Fatal(err)
		}
		n = len(body)
	}
	b.ReportMetric(float64(n), "bytes/batch")
}
