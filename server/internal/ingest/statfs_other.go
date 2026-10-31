//go:build !(linux || darwin)

package ingest

import "errors"

// StatFS is not implemented on this platform; the disk pressure job then
// logs an error on every check and deletes nothing.
func StatFS(string) (DiskUsage, error) {
	return DiskUsage{}, errors.New("disk usage is not supported on this platform")
}
