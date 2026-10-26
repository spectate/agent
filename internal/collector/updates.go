package collector

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spectate/agent/v2/internal/metrics"
	"github.com/spectate/agent/v2/internal/pkgupdates"
)

// maxRebootLabel bounds the `packages` label of system.reboot.required.
const maxRebootLabel = 200

// Updates reports pending package updates and whether a reboot is required.
//
// Update counts come from updates.json, written hourly by the updates helper
// unit (`spectate-agent check-updates`; package managers do not fit in the
// agent's memory limit). Reboot required: /run/reboot-required on
// Debian/Ubuntu (with the packages that asked for it), else what the helper
// learned from `needs-restarting -r` (dnf/yum). Nothing is reported where
// neither is known, e.g. on macOS.
type Updates struct {
	StateDir string
	RunDir   string // default /run (HOST_RUN)
	DpkgDir  string // default /var/lib/dpkg
}

func (*Updates) Name() string { return "updates" }

func (*Updates) IntervalSeconds() int { return 60 }

func (u *Updates) Collect(_ context.Context, now time.Time, emit Emit) error {
	result, err := pkgupdates.Read(u.StateDir)
	known := err == nil && result.Error == "" && !result.CheckedAt.IsZero()
	if known {
		emit("system.updates.pending", nil, float64(result.Pending))
		if result.Security != nil {
			emit("system.updates.security", nil, float64(*result.Security))
		}
		emit("system.updates.checked_age", nil, max(0, now.Sub(result.CheckedAt).Round(time.Second).Seconds()))
	}

	run := orDefault(u.RunDir, runRoot())
	switch {
	case exists(filepath.Join(run, "reboot-required")):
		var labels []metrics.Label
		if pkgs := rebootPackages(filepath.Join(run, "reboot-required.pkgs")); pkgs != "" {
			labels = []metrics.Label{{Key: "packages", Value: pkgs}}
		}
		emit("system.reboot.required", labels, 1)
	case exists(orDefault(u.DpkgDir, "/var/lib/dpkg")):
		emit("system.reboot.required", nil, 0) // Debian/Ubuntu: the file would be there
	case known && result.RebootRequired != nil:
		v := 0.0
		if *result.RebootRequired {
			v = 1
		}
		emit("system.reboot.required", nil, v)
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// rebootPackages joins the distinct packages in /run/reboot-required.pkgs.
func rebootPackages(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if p := strings.TrimSpace(sc.Text()); p != "" {
			seen[p] = true
		}
	}
	pkgs := make([]string, 0, len(seen))
	for p := range seen {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	joined := strings.Join(pkgs, ", ")
	if len(joined) > maxRebootLabel {
		joined = joined[:maxRebootLabel-1] + "…"
	}
	return joined
}
