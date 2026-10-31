//go:build linux || darwin

package ingest

import "syscall"

// StatFS returns the usage of the filesystem that holds path (statfs(2)).
func StatFS(path string) (DiskUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return DiskUsage{}, err
	}
	return NewDiskUsage(st.Blocks, st.Bfree, st.Bavail, uint64(st.Bsize)), nil //nolint:gosec,unconvert // G115: the block size is positive; its type differs per OS
}
