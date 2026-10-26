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

// Sockets reports TCP connection counts and open file handles.
//
// TCP (Linux): established connections from /proc/net/snmp (CurrEstab, IPv4
// and IPv6), sockets in TIME_WAIT, orphaned sockets and sockets in use from
// /proc/net/sockstat (+ sockstat6). These are kernel summaries: no walk over
// every connection, so the cost does not grow with traffic.
//
// File handles: allocated and maximum (/proc/sys/fs/file-nr on Linux,
// kern.num_files and kern.maxfiles on macOS).
type Sockets struct {
	Proc string // procfs root (default /proc or HOST_PROC)
}

func (*Sockets) Name() string { return "sockets" }

func (s *Sockets) Collect(_ context.Context, _ time.Time, emit Emit) error {
	root := orDefault(s.Proc, procRoot())

	if data, err := os.ReadFile(filepath.Join(root, "net", "snmp")); err == nil {
		if v, ok := snmpValue(data, "Tcp", "CurrEstab"); ok {
			emit("system.net.tcp.established", nil, v)
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, "net", "sockstat")); err == nil {
		tcp := sockstatLine(data, "TCP")
		if v, ok := tcp["tw"]; ok {
			emit("system.net.tcp.time_wait", nil, v)
		}
		if v, ok := tcp["orphan"]; ok {
			emit("system.net.tcp.orphaned", nil, v)
		}
		if inuse, ok := tcp["inuse"]; ok {
			if data6, err := os.ReadFile(filepath.Join(root, "net", "sockstat6")); err == nil {
				inuse += sockstatLine(data6, "TCP6")["inuse"]
			}
			emit("system.net.tcp.inuse", nil, inuse)
		}
	}

	if used, maxFiles, ok := fileHandles(root); ok {
		emit("system.files.used", nil, used)
		emit("system.files.max", nil, maxFiles)
	}
	return nil
}

// linuxFileHandles reads /proc/sys/fs/file-nr: allocated, free (always 0
// since Linux 2.6), maximum.
func linuxFileHandles(root string) (used, maxFiles float64, ok bool) {
	data, err := os.ReadFile(filepath.Join(root, "sys", "fs", "file-nr"))
	if err != nil {
		return 0, 0, false
	}
	f := strings.Fields(string(data))
	if len(f) < 3 {
		return 0, 0, false
	}
	allocated, err1 := strconv.ParseFloat(f[0], 64)
	free, err2 := strconv.ParseFloat(f[1], 64)
	limit, err3 := strconv.ParseFloat(f[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, false
	}
	return allocated - free, limit, true
}

// snmpValue reads one counter from /proc/net/snmp, where each protocol has a
// header line of names followed by a line of values.
func snmpValue(data []byte, proto, name string) (float64, bool) {
	var header []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || f[0] != proto+":" {
			continue
		}
		if header == nil {
			header = f
			continue
		}
		for i, h := range header {
			if h == name && i < len(f) {
				v, err := strconv.ParseFloat(f[i], 64)
				return v, err == nil
			}
		}
		return 0, false
	}
	return 0, false
}

// sockstatLine parses a "TCP: inuse 5 orphan 0 tw 2 alloc 7 mem 1" line.
func sockstatLine(data []byte, proto string) map[string]float64 {
	out := map[string]float64{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || f[0] != proto+":" {
			continue
		}
		for i := 1; i+1 < len(f); i += 2 {
			if v, err := strconv.ParseFloat(f[i+1], 64); err == nil {
				out[f[i]] = v
			}
		}
	}
	return out
}
