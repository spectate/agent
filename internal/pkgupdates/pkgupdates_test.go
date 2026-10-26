package pkgupdates

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

type call struct {
	out  string
	code int
}

func fake(calls map[string]call) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, int, error) {
		key := name + " " + strings.Join(args, " ")
		for prefix, c := range calls {
			if strings.HasPrefix(key, prefix) {
				return []byte(c.out), c.code, nil
			}
		}
		return nil, 127, nil
	}
}

func looks(names ...string) func(string) bool {
	return func(name string) bool {
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
}

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func TestAptCountsUpgradableAndSecurity(t *testing.T) {
	out := "Listing...\n" +
		"libc6/jammy-updates,jammy-security 2.35-0ubuntu3.8 amd64 [upgradable from: 2.35-0ubuntu3.7]\n" +
		"curl/jammy-updates 7.81.0-1ubuntu1.18 amd64 [upgradable from: 7.81.0-1ubuntu1.17]\n" +
		"openssl/bookworm-security 3.0.15-1~deb12u1 amd64 [upgradable from: 3.0.14-1~deb12u2]\n"
	r := Checker{
		Run:  fake(map[string]call{"apt list --upgradable": {out, 0}}),
		Look: looks("apt-get", "apt"),
		Now:  func() time.Time { return now },
	}.Check(context.Background())
	if r.Error != "" || r.Manager != "apt" || r.Pending != 3 || r.Security == nil || *r.Security != 2 || !r.CheckedAt.Equal(now) {
		t.Fatalf("apt: %+v", r)
	}
	if r.RebootRequired != nil {
		t.Error("apt: the agent reads /run/reboot-required itself")
	}
}

func TestDnfCountsDistinctPackagesUsesItsCacheAndAsksNeedsRestarting(t *testing.T) {
	var seen []string
	run := func(ctx context.Context, name string, args ...string) ([]byte, int, error) {
		seen = append(seen, name+" "+strings.Join(args, " "))
		return fake(map[string]call{
			"dnf -q --setopt=cachedir=/var/cache/spectate-agent-packages check-update": {"" +
				"bash.x86_64                5.2.15-5.fc40        updates\n" +
				"openssl-libs.x86_64        1:3.2.2-3.fc40       updates\n" +
				"Obsoleting Packages\n" +
				"grub2-tools.x86_64         1:2.06-123.fc40      updates\n", 100},
			"dnf -q --setopt=cachedir=/var/cache/spectate-agent-packages updateinfo list --security": {"" +
				"FEDORA-2024-1 Important/Sec. openssl-libs-1:3.2.2-3.fc40.x86_64\n" +
				"FEDORA-2024-2 Moderate/Sec.  openssl-libs-1:3.2.2-3.fc40.x86_64\n", 0},
			"dnf -q --setopt=cachedir=/var/cache/spectate-agent-packages needs-restarting -r": {"Reboot is required\n", 1},
		})(ctx, name, args...)
	}
	r := Checker{Run: run, Look: looks("dnf"), CacheDir: "/var/cache/spectate-agent-packages", Now: func() time.Time { return now }}.Check(context.Background())
	if r.Error != "" || r.Manager != "dnf" || r.Pending != 2 {
		t.Fatalf("dnf: %+v", r)
	}
	if r.Security == nil || *r.Security != 1 {
		t.Errorf("one package with two advisories: %v", r.Security)
	}
	if r.RebootRequired == nil || !*r.RebootRequired {
		t.Errorf("needs-restarting exit 1: reboot required, got %v", r.RebootRequired)
	}
	for _, c := range seen {
		if !strings.Contains(c, "--setopt=cachedir=") {
			t.Errorf("dnf must use its own cache: %s", c)
		}
	}
}

func TestDnfNothingToUpdateAndErrors(t *testing.T) {
	r := Checker{
		Run:  fake(map[string]call{"dnf -q check-update": {"", 0}, "dnf -q needs-restarting": {"", 0}}),
		Look: looks("dnf"),
	}.Check(context.Background())
	if r.Error != "" || r.Pending != 0 || r.Security != nil || r.RebootRequired == nil || *r.RebootRequired {
		t.Fatalf("up to date: %+v", r)
	}

	r = Checker{Run: fake(map[string]call{"dnf -q check-update": {"Error: Failed to download metadata", 1}}), Look: looks("dnf")}.Check(context.Background())
	if r.Error == "" {
		t.Fatal("exit 1 is an error")
	}

	r = Checker{Look: looks()}.Check(context.Background())
	if r.Error != ErrNoPackageManager.Error() {
		t.Fatalf("no package manager: %+v", r)
	}
}

func TestResultRoundTrip(t *testing.T) {
	dir := t.TempDir()
	n := 4
	want := Result{CheckedAt: now, Manager: "apt", Pending: 9, Security: &n}
	if err := Write(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestParseRPMPackages(t *testing.T) {
	got := ParseRPMPackages([]byte("kernel.x86_64 6.10.5-200.fc40 updates\nnoarch-thing.noarch 1-1 updates\nnot a package line\n"), true)
	if !reflect.DeepEqual(got, []string{"kernel.x86_64", "noarch-thing.noarch"}) {
		t.Fatalf("got %v", got)
	}
}
