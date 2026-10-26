//go:build darwin

package collector

import "golang.org/x/sys/unix"

// fileHandles reads kern.num_files and kern.maxfiles.
func fileHandles(string) (used, maxFiles float64, ok bool) {
	n, err1 := unix.SysctlUint32("kern.num_files")
	m, err2 := unix.SysctlUint32("kern.maxfiles")
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return float64(n), float64(m), true
}
