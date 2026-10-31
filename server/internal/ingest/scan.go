package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// Scan walks the recordings directory and enqueues complete segments that
// are not queued yet (missed notifications, segments recorded while the API
// was down). It returns how many rows it added.
//
// A segment that may still be written is skipped. MediaMTX writes one
// segment at a time per path, so only the newest file of a path can be in
// progress: it is taken only when its mtime is older than StaleAge (the
// recording stopped). Every file must also be older than MinAge.
//
// Only cam_<uuid>_main directories of registered cameras are considered;
// symlinks are ignored.
func (s *Service) Scan(ctx context.Context) (int, error) {
	root := s.o.Layout.Root()
	dirs, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("ingest: read recordings dir: %w", err)
	}
	now := s.o.Now()
	added := 0
	var errs []error
	for _, d := range dirs {
		if ctx.Err() != nil {
			return added, ctx.Err()
		}
		if !d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			continue
		}
		camID, ok := ParsePathName(d.Name())
		if !ok {
			continue
		}
		exists, err := s.o.Queue.CameraExists(ctx, camID)
		if err != nil {
			return added, fmt.Errorf("ingest: look up camera: %w", err)
		}
		if !exists {
			s.log.Debug("skipping recordings of an unknown camera", slog.String("dir", d.Name()))
			continue
		}
		n, err := s.scanDir(ctx, filepath.Join(root, d.Name()), camID, now)
		added += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return added, errors.Join(errs...)
}

type candidate struct {
	path  string
	start time.Time
	mtime time.Time
}

func (s *Service) scanDir(ctx context.Context, dir string, camID uuid.UUID, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("ingest: read %s: %w", dir, err)
	}
	var segs []candidate
	var newest time.Time
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		start, ok := ParseSegmentName(e.Name(), s.o.Layout.location)
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed meanwhile
		}
		segs = append(segs, candidate{path: filepath.Join(dir, e.Name()), start: start, mtime: info.ModTime()})
		if start.After(newest) {
			newest = start
		}
	}
	added := 0
	for _, c := range segs {
		age := now.Sub(c.mtime)
		if age < s.o.MinAge || (c.start.Equal(newest) && age < s.o.StaleAge) {
			continue
		}
		seg, err := s.o.Layout.Resolve(c.path)
		if err != nil {
			if !errors.Is(err, ErrSegmentNotFound) {
				s.log.Warn("skipping invalid segment", slog.String("local_path", c.path), slog.String("error", err.Error()))
			}
			continue
		}
		if seg.CameraID != camID {
			continue
		}
		ok, err := s.o.Queue.Enqueue(ctx, seg.LocalPath)
		if err != nil {
			return added, fmt.Errorf("ingest: enqueue: %w", err)
		}
		if ok {
			added++
		}
	}
	return added, nil
}
