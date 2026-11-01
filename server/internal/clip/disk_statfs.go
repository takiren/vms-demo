//go:build linux || darwin

package clip

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// freeSpace returns the bytes available to unprivileged users on the file
// system of dir.
func freeSpace(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // G115: block sizes are small positive numbers
}

// checkOwner refuses a directory owned by another user.
func checkOwner(fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != os.Getuid() {
		return errors.New("is owned by another user")
	}
	return nil
}
