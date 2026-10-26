package collector

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/spectate/agent/v2/internal/config"
	"github.com/spectate/agent/v2/internal/metrics"
)

type sample struct {
	labels []metrics.Label
	value  float64
}

func record() (Emit, map[string][]sample) {
	got := map[string][]sample{}
	return func(m string, l []metrics.Label, v float64) { got[m] = append(got[m], sample{l, v}) }, got
}

func TestCPUPercentagesMatchV1Formula(t *testing.T) {
	prev := cpu.TimesStat{User: 100, Nice: 0, System: 50, Idle: 800, Iowait: 10, Irq: 5, Softirq: 5, Steal: 0, Guest: 0}
	cur := cpu.TimesStat{User: 130, Nice: 0, System: 60, Idle: 850, Iowait: 15, Irq: 7, Softirq: 8, Steal: 0, Guest: 0}
	emit, got := record()
	CPUPercentages(prev, cur, emit)

	// elapsed = 100 ticks
	want := map[string]float64{
		"system.cpu.user":      30,
		"system.cpu.system":    15, // system + irq + softirq, as in v1
		"system.cpu.interrupt": 5,
		"system.cpu.iowait":    5,
		"system.cpu.idle":      50,
		"system.cpu.stolen":    0,
		"system.cpu.guest":     0,
	}
	for k, v := range want {
		if len(got[k]) != 1 || got[k][0].value != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestNetDeltasNormaliseToPerMinute(t *testing.T) {
	prev := map[string]net.IOCountersStat{
		"eth0":    {Name: "eth0", BytesRecv: 1024 * 100, PacketsRecv: 10},
		"docker0": {Name: "docker0"},
		"eth1":    {Name: "eth1", BytesRecv: 5000},
	}
	cur := []net.IOCountersStat{
		{Name: "eth0", BytesRecv: 1024*100 + 1024*25, PacketsRecv: 20},
		{Name: "docker0", BytesRecv: 999},
		{Name: "eth1", BytesRecv: 10}, // counter reset
		{Name: "eth2", BytesRecv: 10}, // new interface, no baseline
	}
	n := &Net{}
	n.Configure(config.DefaultRemote())
	emit, got := record()
	NetDeltas(prev, cur, 15*time.Second, n.Excluded, emit)

	recv := got["system.net.bytes_recv"]
	if len(recv) != 1 || recv[0].labels[0].Value != "eth0" {
		t.Fatalf("only eth0 should report, got %+v", recv)
	}
	if recv[0].value != 100 { // 25 KB in 15s = 100 KB per minute
		t.Fatalf("bytes_recv = %v KB/min, want 100", recv[0].value)
	}
	if got["system.net.packets_recv"][0].value != 40 {
		t.Fatalf("packets_recv = %v, want 40", got["system.net.packets_recv"][0].value)
	}
	if len(got["system.net.fifo_out"]) != 1 {
		t.Fatal("all ten v1 net metrics must be emitted")
	}
}

func TestNetFilterSemantics(t *testing.T) {
	n := &Net{}
	n.Configure(config.DefaultRemote())
	for iface, excluded := range map[string]bool{
		"eth0": false, "en0": false, "lo0": false, // explicitly included
		"docker0": true, "veth12ab": true, "lo": true, "virbr0": true, // wildcard / exact excludes
		"ens3": false, "enp0s3": false, "eno1": false, "wlan0": false, "bond0": false, // v1 hid these via en*
	} {
		if n.Excluded(iface) != excluded {
			t.Errorf("Excluded(%q) = %v, want %v", iface, !excluded, excluded)
		}
	}
}

func TestDiskFilters(t *testing.T) {
	d := &Disk{}
	cfg := config.DefaultRemote()
	cfg.Disk.ExcludeMountpoints = []string{"/snap*"}
	d.Configure(cfg)
	cases := []struct {
		p    disk.PartitionStat
		want bool
	}{
		{disk.PartitionStat{Mountpoint: "/", Device: "/dev/sda1", Fstype: "ext4"}, false},
		{disk.PartitionStat{Mountpoint: "/boot/efi", Device: "/dev/sda2", Fstype: "VFAT"}, true},
		{disk.PartitionStat{Mountpoint: "/snap/core/1", Device: "/dev/loop0", Fstype: "x"}, true},
	}
	for _, c := range cases {
		if d.Excluded(c.p) != c.want {
			t.Errorf("Excluded(%v) = %v", c.p, !c.want)
		}
	}
	cfg.Disk.IncludeMountpoints = []string{"/data"}
	d.Configure(cfg)
	if !d.Excluded(disk.PartitionStat{Mountpoint: "/"}) || d.Excluded(disk.PartitionStat{Mountpoint: "/data"}) {
		t.Fatal("include_mountpoints should restrict to the listed mounts")
	}
}

// TestSystemCollectorsRunOnThisHost is a smoke test: every collector must run
// without error and emit finite, v1-named metrics on the build host.
func TestSystemCollectorsRunOnThisHost(t *testing.T) {
	ctx := context.Background()
	emit, got := record()
	for _, c := range System(t.TempDir()) {
		if cc, ok := c.(Configurable); ok {
			cc.Configure(config.DefaultRemote())
		}
		for round := range 2 { // second round yields CPU/net deltas
			if round == 1 {
				time.Sleep(300 * time.Millisecond)
			}
			if err := c.Collect(ctx, time.Now(), emit); err != nil {
				t.Fatalf("%s: %v", c.Name(), err)
			}
		}
	}
	for _, m := range []string{"system.cpu.idle", "system.memory.total", "system.load.1", "system.uptime"} {
		if len(got[m]) == 0 {
			t.Errorf("missing %s", m)
		}
	}
	info, err := HostInfo(ctx)
	if err != nil || Hostname(info) == "" {
		t.Fatalf("host info: %v", err)
	}
}

func TestDefaultsExcludeNetworkFilesystems(t *testing.T) {
	d := &Disk{}
	d.Configure(config.DefaultRemote())
	for _, fs := range []string{"nfs", "nfs4", "cifs", "smbfs", "smb3", "afpfs", "webdav", "autofs", "fuse.sshfs", "ceph", "glusterfs", "fuse.glusterfs", "9p"} {
		if !d.Excluded(disk.PartitionStat{Mountpoint: "/mnt/x", Fstype: fs}) {
			t.Errorf("%s should be excluded by default", fs)
		}
	}
	for _, fs := range []string{"ext4", "xfs", "btrfs", "zfs", "apfs"} {
		if d.Excluded(disk.PartitionStat{Mountpoint: "/data", Fstype: fs}) {
			t.Errorf("%s should be collected", fs)
		}
	}
}

// A mountpoint whose statfs hangs (dead NFS server) must not hide the other
// disks, and must not be queried again while the first call is stuck.
func TestDiskSkipsHungMountpoint(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var mu sync.Mutex
	calls := map[string]int{}

	d := &Disk{UsageTimeout: 50 * time.Millisecond}
	d.Configure(config.DefaultRemote())
	d.partitions = func(context.Context, bool) ([]disk.PartitionStat, error) {
		return []disk.PartitionStat{
			{Mountpoint: "/", Device: "/dev/sda1", Fstype: "ext4"},
			{Mountpoint: "/mnt/dead", Device: "/dev/sdc1", Fstype: "ext4"},
			{Mountpoint: "/data", Device: "/dev/sdb1", Fstype: "xfs"},
		}, nil
	}
	d.usage = func(_ context.Context, path string) (*disk.UsageStat, error) {
		mu.Lock()
		calls[path]++
		mu.Unlock()
		if path == "/mnt/dead" {
			<-release // uninterruptible, ignores ctx
		}
		return &disk.UsageStat{Path: path, Total: 100, Used: 40, Free: 60, UsedPercent: 40}, nil
	}

	for round := range 3 {
		emit, got := record()
		start := time.Now()
		if err := d.Collect(context.Background(), time.Now(), emit); err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)
		if round == 0 && took > time.Second {
			t.Fatalf("collect took %v", took)
		}
		if round > 0 && took > 40*time.Millisecond {
			t.Fatalf("round %d waited %v on the hung mount again", round, took)
		}
		var mounts []string
		for _, s := range got["system.disk_usage.total"] {
			mounts = append(mounts, s.labels[0].Value)
		}
		if len(mounts) != 2 || mounts[0] != "/" || mounts[1] != "/data" {
			t.Fatalf("round %d: mounts = %v, want [/ /data]", round, mounts)
		}
	}
	mu.Lock()
	if calls["/mnt/dead"] != 1 || calls["/"] != 3 {
		t.Fatalf("calls = %v: the hung mount must be queried once", calls)
	}
	mu.Unlock()
	if hung := d.Hung(); len(hung) != 1 || hung[0] != "/mnt/dead" {
		t.Fatalf("Hung() = %v", hung)
	}

	// Once the call returns, the mountpoint is collected again.
	unblock()
	for deadline := time.Now().Add(5 * time.Second); len(d.Hung()) > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("hung call never cleared")
		}
	}
	emit, got := record()
	if err := d.Collect(context.Background(), time.Now(), emit); err != nil {
		t.Fatal(err)
	}
	if n := len(got["system.disk_usage.total"]); n != 3 {
		t.Fatalf("after recovery: %d mounts, want 3", n)
	}
}
