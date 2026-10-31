//go:build !linux && !darwin

package clip

import (
	"errors"
	"io/fs"
)

func freeSpace(string) (uint64, error) {
	return 0, errors.New("clip: free space is unknown on this platform")
}

func checkOwner(fs.FileInfo) error { return nil }
