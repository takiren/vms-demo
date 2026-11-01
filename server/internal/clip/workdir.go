package clip

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// workDirPattern matches the per-request directories os.MkdirTemp creates
// with workDirPrefix + "*". The sweep removes nothing else.
var workDirPattern = regexp.MustCompile(`^clip-[0-9]{1,20}$`)

const workDirPrefix = "clip-"

// workDirs owns the base directory of clip jobs: it creates one directory
// per request, remembers which are in use, and sweeps leftovers (from a
// crash or a kill) that are older than staleAge and not in use.
type workDirs struct {
	base     string
	staleAge time.Duration
	now      func() time.Time

	mu     sync.Mutex
	active map[string]struct{}
}

// prepareBase creates base (0700) if needed and refuses one that is a
// symbolic link, not a directory, or (on Unix) owned by someone else: the
// default lives under the shared temporary directory.
func prepareBase(base string) error {
	if !filepath.IsAbs(base) {
		return fmt.Errorf("clip: temporary directory %q is not absolute", base)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return fmt.Errorf("clip: create temporary directory: %w", err)
	}
	fi, err := os.Lstat(base)
	if err != nil {
		return fmt.Errorf("clip: temporary directory: %w", err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("clip: temporary directory %q is not a directory (or is a symbolic link)", base)
	}
	if err := checkOwner(fi); err != nil {
		return fmt.Errorf("clip: temporary directory %q: %w", base, err)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(base, 0o700); err != nil { //nolint:gosec // G302: a directory needs the x bit; owner only
			return fmt.Errorf("clip: temporary directory: %w", err)
		}
	}
	return nil
}

// create makes a new job directory and marks it in use.
func (w *workDirs) create() (string, error) {
	dir, err := os.MkdirTemp(w.base, workDirPrefix+"*")
	if err != nil {
		return "", fmt.Errorf("clip: create job directory: %w", err)
	}
	w.mu.Lock()
	w.active[filepath.Base(dir)] = struct{}{}
	w.mu.Unlock()
	return dir, nil
}

// remove deletes a job directory and forgets it.
func (w *workDirs) remove(dir string) error {
	err := os.RemoveAll(dir)
	w.mu.Lock()
	delete(w.active, filepath.Base(dir))
	w.mu.Unlock()
	return err
}

// sweep removes job directories older than staleAge that are not in use.
// It never follows symbolic links: entries are classified without
// following them (ReadDir uses lstat-like types), and os.RemoveAll removes
// links inside a directory rather than their targets.
func (w *workDirs) sweep() (removed int, err error) {
	entries, err := os.ReadDir(w.base)
	if err != nil {
		return 0, fmt.Errorf("clip: sweep: %w", err)
	}
	cutoff := w.now().Add(-w.staleAge)
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if !workDirPattern.MatchString(name) || e.Type()&fs.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		w.mu.Lock()
		_, inUse := w.active[name]
		w.mu.Unlock()
		if inUse {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(w.base, name)); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
