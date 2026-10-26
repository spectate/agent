package collector

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// procRoot is where procfs is mounted: /proc, or HOST_PROC (as gopsutil
// honours it) when the agent runs in a container with the host's /proc
// mounted elsewhere.
func procRoot() string {
	if v := os.Getenv("HOST_PROC"); v != "" {
		return v
	}
	return "/proc"
}

// sysRoot is where sysfs is mounted (HOST_SYS, as gopsutil honours it).
func sysRoot() string {
	if v := os.Getenv("HOST_SYS"); v != "" {
		return v
	}
	return "/sys"
}

// runRoot is /run (HOST_RUN, for containers).
func runRoot() string {
	if v := os.Getenv("HOST_RUN"); v != "" {
		return v
	}
	return "/run"
}

func orDefault(path, fallback string) string {
	if path != "" {
		return path
	}
	return fallback
}

// fields parses "key value" lines (/proc/vmstat) into a map.
func fields(data []byte) map[string]float64 {
	out := make(map[string]float64)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		if v, err := strconv.ParseFloat(f[1], 64); err == nil {
			out[f[0]] = v
		}
	}
	return out
}

// Runner runs a command and returns its standard output. Commands run with
// LC_ALL=C so their output does not depend on the host's language.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Exec is the default Runner.
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	return cmd.Output()
}

// systemdBooted reports whether the host runs systemd as init
// (sd_booted(3): /run/systemd/system exists).
func systemdBooted(run string) bool {
	info, err := os.Stat(filepath.Join(run, "systemd", "system"))
	return err == nil && info.IsDir()
}
