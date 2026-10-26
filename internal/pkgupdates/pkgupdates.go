// Package pkgupdates checks for pending package updates with the host's
// package manager (apt, dnf or yum) and stores the result for the agent.
//
// Package managers need far more memory than the agent's 64 MB unit allows,
// and dnf needs a writable cache, so the check runs from its own hourly unit
// (`spectate-agent check-updates`, spectate-agent-updates.timer) and writes
// updates.json into the state directory; the agent's updates collector reads
// it. The check never refreshes the system's package lists (apt) and keeps
// dnf's metadata in the unit's own cache directory.
package pkgupdates

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileName is the result file in the state directory.
const FileName = "updates.json"

// Result is one check's outcome.
type Result struct {
	CheckedAt time.Time `json:"checked_at"`
	Manager   string    `json:"manager,omitempty"` // apt, dnf, yum
	Pending   int       `json:"pending"`
	// Security is the number of pending updates that fix security issues;
	// nil when the package manager cannot tell.
	Security *int `json:"security,omitempty"`
	// RebootRequired is set where the package manager knows (dnf/yum
	// needs-restarting); on Debian/Ubuntu the agent reads /run/reboot-required.
	RebootRequired *bool  `json:"reboot_required,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Path returns the result file path in dir.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// Read loads the last result.
func Read(dir string) (Result, error) {
	var r Result
	data, err := os.ReadFile(Path(dir))
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(data, &r)
	return r, err
}

// Write stores a result atomically.
func Write(dir string, r Result) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path(dir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path(dir))
}

// Runner runs a command: its standard output and exit status. err is set only
// when the command could not run at all.
type Runner func(ctx context.Context, name string, args ...string) (out []byte, code int, err error)

// Exec is the default Runner (LC_ALL=C).
func Exec(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out, exit.ExitCode(), nil
	}
	if err != nil {
		return out, -1, err
	}
	return out, 0, nil
}

// Checker runs the check. Zero values use the host.
type Checker struct {
	Run      Runner
	Look     func(name string) bool // command available (default exec.LookPath)
	CacheDir string                 // dnf/yum metadata cache (systemd CacheDirectory)
	Now      func() time.Time
}

// ErrNoPackageManager is reported when neither apt, dnf nor yum is installed.
var ErrNoPackageManager = errors.New("no supported package manager (apt, dnf, yum)")

// Check counts pending and security updates.
func (c Checker) Check(ctx context.Context) Result {
	if c.Run == nil {
		c.Run = Exec
	}
	if c.Look == nil {
		c.Look = func(name string) bool { _, err := exec.LookPath(name); return err == nil }
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	var r Result
	var err error
	switch {
	case c.Look("apt-get") && c.Look("apt"):
		r, err = c.apt(ctx)
	case c.Look("dnf"):
		r, err = c.rpm(ctx, "dnf")
	case c.Look("yum"):
		r, err = c.rpm(ctx, "yum")
	default:
		err = ErrNoPackageManager
	}
	r.CheckedAt = c.Now().UTC()
	if err != nil {
		r.Error = err.Error()
	}
	return r
}

// apt lists upgradable packages from the lists already on disk (whatever
// refreshes them, e.g. apt-daily.timer); it never runs `apt-get update`.
func (c Checker) apt(ctx context.Context) (Result, error) {
	r := Result{Manager: "apt"}
	out, code, err := c.Run(ctx, "apt", "list", "--upgradable")
	if err != nil {
		return r, err
	}
	if code != 0 {
		return r, errors.New("apt list --upgradable failed")
	}
	pending, security := ParseAptUpgradable(out)
	r.Pending, r.Security = pending, &security
	return r, nil
}

// ParseAptUpgradable counts `apt list --upgradable` lines, and those whose
// suites include a -security one (bookworm-security, jammy-security).
func ParseAptUpgradable(out []byte) (pending, security int) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "[upgradable from") {
			continue
		}
		pending++
		name, _, _ := strings.Cut(line, " ")
		if _, suites, ok := strings.Cut(name, "/"); ok {
			for _, suite := range strings.Split(suites, ",") {
				if strings.HasSuffix(suite, "-security") {
					security++
					break
				}
			}
		}
	}
	return pending, security
}

func (c Checker) rpm(ctx context.Context, manager string) (Result, error) {
	r := Result{Manager: manager}
	common := []string{"-q"}
	if c.CacheDir != "" {
		common = append(common, "--setopt=cachedir="+c.CacheDir)
	}

	// check-update: 0 = nothing to update, 100 = updates listed, else an error
	out, code, err := c.Run(ctx, manager, append(append([]string{}, common...), "check-update")...)
	if err != nil {
		return r, err
	}
	if code != 0 && code != 100 {
		return r, errors.New(manager + " check-update failed")
	}
	r.Pending = len(ParseRPMPackages(out, true))

	if out, code, err := c.Run(ctx, manager, append(append([]string{}, common...), "updateinfo", "list", "--security")...); err == nil && code == 0 {
		n := len(ParseRPMPackages(out, false))
		r.Security = &n
	}

	r.RebootRequired = c.needsRestarting(ctx, manager, common)
	return r, nil
}

// needsRestarting asks `needs-restarting -r`: exit 1 means a reboot is
// required, 0 that none is. nil when the tool is not installed.
func (c Checker) needsRestarting(ctx context.Context, manager string, common []string) *bool {
	try := func(name string, args ...string) *bool {
		_, code, err := c.Run(ctx, name, args...)
		if err != nil || (code != 0 && code != 1) {
			return nil
		}
		required := code == 1
		return &required
	}
	if v := try(manager, append(append([]string{}, common...), "needs-restarting", "-r")...); v != nil {
		return v
	}
	if c.Look("needs-restarting") {
		return try("needs-restarting", "-r")
	}
	return nil
}

var rpmArches = []string{".x86_64", ".noarch", ".aarch64", ".i686", ".ppc64le", ".s390x", ".armv7hl"}

// ParseRPMPackages returns the distinct packages in dnf/yum output: fields
// that end in an architecture (bash.x86_64, openssl-3.0.9-2.fc38.x86_64).
// With stopAtObsoletes the "Obsoleting Packages" section of check-update is
// ignored (those packages are already counted above it).
func ParseRPMPackages(out []byte, stopAtObsoletes bool) []string {
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if stopAtObsoletes && strings.HasPrefix(strings.TrimSpace(line), "Obsoleting") {
			break
		}
		for _, f := range strings.Fields(line) {
			for _, arch := range rpmArches {
				if strings.HasSuffix(f, arch) && len(f) > len(arch) {
					seen[f] = true
				}
			}
		}
	}
	out2 := make([]string, 0, len(seen))
	for p := range seen {
		out2 = append(out2, p)
	}
	sort.Strings(out2)
	return out2
}
