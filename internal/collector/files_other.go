//go:build !darwin

package collector

func fileHandles(root string) (used, maxFiles float64, ok bool) {
	return linuxFileHandles(root)
}
