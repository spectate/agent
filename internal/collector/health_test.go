package collector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/spectate/agent/v2/internal/config"
	"github.com/spectate/agent/v2/internal/pkgupdates"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func only(t *testing.T, got map[string][]sample, metric string) float64 {
	t.Helper()
	if len(got[metric]) != 1 {
		t.Fatalf("%s: want 1 sample, got %v", metric, got[metric])
	}
	return got[metric][0].value
}

func psi(someTotal, fullTotal int) string {
	return "some avg10=1.00 avg60=1.00 avg300=1.00 total=" + strconv.Itoa(someTotal) + "\n" +
		"full avg10=0.00 avg60=0.00 avg300=0.00 total=" + strconv.Itoa(fullTotal) + "\n"
}

func TestPressureAndOOMKillsAreRatesOverTheRound(t *testing.T) {
	proc := t.TempDir()
	p := &Pressure{Proc: proc}
	t0 := time.Unix(1_700_000_000, 0)
	writeFiles(t, proc, map[string]string{
		"pressure/cpu":    psi(1_000_000, 0),
		"pressure/memory": psi(0, 0),
		"pressure/io":     psi(5_000_000, 2_000_000),
		"vmstat":          "nr_free_pages 1234\noom_kill 7\n",
	})
	emit, got := record()
	if err := p.Collect(context.Background(), t0, emit); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("first round only primes, got %v", got)
	}

	// 10s later: cpu stalled 2.5s (25%), memory 1s some / 0.5s full, io 0
	writeFiles(t, proc, map[string]string{
		"pressure/cpu":    psi(3_500_000, 0),
		"pressure/memory": psi(1_000_000, 500_000),
		"pressure/io":     psi(5_000_000, 2_000_000),
		"vmstat":          "nr_free_pages 1234\noom_kill 9\n",
	})
	if err := p.Collect(context.Background(), t0.Add(10*time.Second), emit); err != nil {
		t.Fatal(err)
	}
	for metric, want := range map[string]float64{
		"system.pressure.cpu.some":    25,
		"system.pressure.memory.some": 10,
		"system.pressure.memory.full": 5,
		"system.pressure.io.some":     0,
		"system.pressure.io.full":     0,
		"system.oom.kills":            2,
	} {
		if v := only(t, got, metric); v != want {
			t.Errorf("%s = %v, want %v", metric, v, want)
		}
	}
	if len(got["system.pressure.cpu.full"]) != 0 {
		t.Error("cpu full is always zero at the system level and is not reported")
	}
}

func TestPressureReportsNothingWithoutPSI(t *testing.T) {
	p := &Pressure{Proc: t.TempDir()}
	emit, got := record()
	for i := range 2 {
		if err := p.Collect(context.Background(), time.Unix(int64(i*15), 0), emit); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 0 {
		t.Fatalf("no /proc/pressure or vmstat: nothing to report, got %v", got)
	}
}

func TestSocketsReadKernelSummaries(t *testing.T) {
	proc := t.TempDir()
	writeFiles(t, proc, map[string]string{
		"net/snmp": "Ip: Forwarding DefaultTTL\nIp: 1 64\n" +
			"Tcp: RtoAlgorithm RtoMin RtoMax MaxConn ActiveOpens PassiveOpens AttemptFails EstabResets CurrEstab InSegs\n" +
			"Tcp: 1 200 120000 -1 100 200 3 4 42 9999\n",
		"net/sockstat":     "sockets: used 300\nTCP: inuse 50 orphan 2 tw 17 alloc 60 mem 5\nUDP: inuse 3 mem 1\n",
		"net/sockstat6":    "TCP6: inuse 8\nUDP6: inuse 1\n",
		"sys/fs/file-nr":   "4096\t0\t9223372036854775807\n",
		"unrelated/ignore": "x",
	})
	emit, got := record()
	if err := (&Sockets{Proc: proc}).Collect(context.Background(), time.Now(), emit); err != nil {
		t.Fatal(err)
	}
	for metric, want := range map[string]float64{
		"system.net.tcp.established": 42,
		"system.net.tcp.time_wait":   17,
		"system.net.tcp.orphaned":    2,
		"system.net.tcp.inuse":       58,
	} {
		if v := only(t, got, metric); v != want {
			t.Errorf("%s = %v, want %v", metric, v, want)
		}
	}
	if len(got["system.files.used"]) != 1 || len(got["system.files.max"]) != 1 {
		t.Error("file handles are reported (file-nr on Linux, sysctl on macOS)")
	}
	used, limit, ok := linuxFileHandles(proc)
	if !ok || used != 4096 || limit != 9223372036854775807 {
		t.Errorf("file-nr: used %v max %v ok %v", used, limit, ok)
	}
}

func fakeRun(outputs map[string]string, errs map[string]error) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		for prefix, err := range errs {
			if strings.HasPrefix(key, prefix) {
				return nil, err
			}
		}
		for prefix, out := range outputs {
			if strings.HasPrefix(key, prefix) {
				return []byte(out), nil
			}
		}
		return nil, errors.New("unexpected command: " + key)
	}
}

func systemdRunDir(t *testing.T) string {
	run := t.TempDir()
	if err := os.MkdirAll(filepath.Join(run, "systemd", "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestServicesNameEveryFailedUnit(t *testing.T) {
	s := &Services{
		RunDir: systemdRunDir(t),
		Run: fakeRun(map[string]string{"systemctl list-units": "" +
			"php-fpm.service loaded failed failed The PHP FastCGI Process Manager\n" +
			"● nginx.service loaded failed failed A high performance web server\n"}, nil),
	}
	emit, got := record()
	if err := s.Collect(context.Background(), time.Now(), emit); err != nil {
		t.Fatal(err)
	}
	if v := only(t, got, "system.services.failed"); v != 2 {
		t.Fatalf("failed = %v", v)
	}
	var units []string
	for _, s := range got["system.service.failed"] {
		units = append(units, s.labels[0].Value)
	}
	if !reflect.DeepEqual(units, []string{"nginx.service", "php-fpm.service"}) {
		t.Fatalf("units = %v", units)
	}
	if s.IntervalSeconds() != 60 {
		t.Error("systemctl runs once a minute")
	}
}

func TestServicesReportsZeroWhenAllIsWellAndNothingWithoutSystemd(t *testing.T) {
	emit, got := record()
	ok := &Services{RunDir: systemdRunDir(t), Run: fakeRun(map[string]string{"systemctl": ""}, nil)}
	if err := ok.Collect(context.Background(), time.Now(), emit); err != nil {
		t.Fatal(err)
	}
	if v := only(t, got, "system.services.failed"); v != 0 {
		t.Fatalf("failed = %v", v)
	}

	emit, got = record()
	none := &Services{RunDir: t.TempDir(), Run: fakeRun(nil, map[string]error{"": errors.New("must not run")})}
	if err := none.Collect(context.Background(), time.Now(), emit); err != nil || len(got) != 0 {
		t.Fatalf("no systemd: nothing to report, got %v %v", got, err)
	}
}

func TestClockSynced(t *testing.T) {
	for out, want := range map[string]float64{"yes\n": 1, "no\n": 0} {
		emit, got := record()
		c := &Clock{RunDir: systemdRunDir(t), Run: fakeRun(map[string]string{"timedatectl show": out}, nil)}
		if err := c.Collect(context.Background(), time.Now(), emit); err != nil {
			t.Fatal(err)
		}
		if v := only(t, got, "system.clock.synced"); v != want {
			t.Errorf("%q: synced = %v", out, v)
		}
	}
	emit, got := record()
	c := &Clock{RunDir: systemdRunDir(t), Run: fakeRun(nil, map[string]error{"timedatectl": errors.New("no timedated")})}
	if err := c.Collect(context.Background(), time.Now(), emit); err != nil || len(got) != 0 {
		t.Fatalf("unknown: nothing to report, got %v %v", got, err)
	}
}

func TestUpdatesFromTheHelperAndRebootRequired(t *testing.T) {
	state, run, dpkg := t.TempDir(), t.TempDir(), t.TempDir()
	security := 3
	now := time.Unix(1_700_000_000, 0)
	if err := pkgupdates.Write(state, pkgupdates.Result{CheckedAt: now.Add(-40 * time.Minute), Manager: "apt", Pending: 12, Security: &security}); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, run, map[string]string{
		"reboot-required":      "*** System restart required ***\n",
		"reboot-required.pkgs": "linux-image-6.8.0-45-generic\nlibc6\nlibc6\n",
	})
	emit, got := record()
	u := &Updates{StateDir: state, RunDir: run, DpkgDir: dpkg}
	if err := u.Collect(context.Background(), now, emit); err != nil {
		t.Fatal(err)
	}
	for metric, want := range map[string]float64{
		"system.updates.pending":     12,
		"system.updates.security":    3,
		"system.updates.checked_age": 2400,
		"system.reboot.required":     1,
	} {
		if v := only(t, got, metric); v != want {
			t.Errorf("%s = %v, want %v", metric, v, want)
		}
	}
	if l := got["system.reboot.required"][0].labels; len(l) != 1 || l[0].Value != "libc6, linux-image-6.8.0-45-generic" {
		t.Errorf("packages label = %v", l)
	}

	// Debian without the file: no reboot needed
	emit, got = record()
	if err := (&Updates{StateDir: t.TempDir(), RunDir: t.TempDir(), DpkgDir: dpkg}).Collect(context.Background(), now, emit); err != nil {
		t.Fatal(err)
	}
	if v := only(t, got, "system.reboot.required"); v != 0 {
		t.Errorf("reboot = %v", v)
	}
	if len(got["system.updates.pending"]) != 0 {
		t.Error("no helper result yet: no update counts")
	}

	// RHEL: what needs-restarting told the helper; macOS: nothing
	required := true
	state2 := t.TempDir()
	_ = pkgupdates.Write(state2, pkgupdates.Result{CheckedAt: now, Manager: "dnf", Pending: 1, RebootRequired: &required})
	emit, got = record()
	if err := (&Updates{StateDir: state2, RunDir: t.TempDir(), DpkgDir: filepath.Join(t.TempDir(), "none")}).Collect(context.Background(), now, emit); err != nil {
		t.Fatal(err)
	}
	if v := only(t, got, "system.reboot.required"); v != 1 {
		t.Errorf("rhel reboot = %v", v)
	}
	if len(got["system.updates.security"]) != 0 {
		t.Error("unknown security count is not reported as 0")
	}

	emit, got = record()
	if err := (&Updates{StateDir: t.TempDir(), RunDir: t.TempDir(), DpkgDir: filepath.Join(t.TempDir(), "none")}).Collect(context.Background(), now, emit); err != nil || len(got) != 0 {
		t.Fatalf("nothing known: nothing to report, got %v", got)
	}
}

func TestDiskIOPerWholeDiskPerSecond(t *testing.T) {
	sys := t.TempDir()
	for _, name := range []string{"sda", "nvme0n1", "loop0", "dm-0"} {
		if err := os.MkdirAll(filepath.Join(sys, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rounds := []map[string]disk.IOCountersStat{
		{
			"sda":       {ReadCount: 100, WriteCount: 100, ReadBytes: 0, WriteBytes: 0, ReadTime: 0, WriteTime: 0, IoTime: 0},
			"sda1":      {ReadCount: 100},
			"nvme0n1":   {ReadCount: 10},
			"loop0":     {ReadCount: 1},
			"dm-0":      {ReadCount: 1},
			"nvme0n1p1": {ReadCount: 10},
		},
		{
			// 10s: 200 reads + 100 writes, 10 MB read, 5 MB written, 900ms of op time, 5s busy
			"sda":       {ReadCount: 300, WriteCount: 200, ReadBytes: 10_000_000, WriteBytes: 5_000_000, ReadTime: 600, WriteTime: 300, IoTime: 5000},
			"sda1":      {ReadCount: 300},
			"nvme0n1":   {ReadCount: 5}, // went backwards: re-attached, skipped this round
			"loop0":     {ReadCount: 9},
			"dm-0":      {ReadCount: 9},
			"nvme0n1p1": {ReadCount: 20},
		},
	}
	i := 0
	d := &DiskIO{sysBlock: sys, counters: func(context.Context) (map[string]disk.IOCountersStat, error) {
		r := rounds[i]
		i++
		return r, nil
	}}
	d.Configure(config.DefaultRemote())
	emit, got := record()
	t0 := time.Unix(1_700_000_000, 0)
	_ = d.Collect(context.Background(), t0, emit)
	_ = d.Collect(context.Background(), t0.Add(10*time.Second), emit)

	for metric, want := range map[string]float64{
		"system.io.read_bytes":   1_000_000,
		"system.io.write_bytes":  500_000,
		"system.io.reads":        20,
		"system.io.writes":       10,
		"system.io.busy_percent": 50,
		"system.io.await_ms":     3,
	} {
		if v := only(t, got, metric); v != want {
			t.Errorf("%s = %v, want %v", metric, v, want)
		}
	}
	if got["system.io.reads"][0].labels[0].Value != "sda" {
		t.Errorf("only sda (partitions, loop, dm and a reset device are skipped): %v", got["system.io.reads"])
	}
}

func TestUnits(t *testing.T) {
	for metric, want := range map[string]string{
		"system.cpu.user":            "%",
		"system.memory.total":        "MB",
		"system.disk_usage.used":     "KB",
		"system.net.bytes_recv":      "KB/60s",
		"system.net.tcp.established": "sockets",
		"system.io.read_bytes":       "B/s",
		"system.io.busy_percent":     "%",
		"system.pressure.io.some":    "%",
		"system.reboot.required":     "0/1",
		"system.updates.pending":     "packages",
	} {
		if got := Unit(metric); got != want {
			t.Errorf("Unit(%s) = %q, want %q", metric, got, want)
		}
	}
}
