package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
)

// Disk pressure (docs/design.md「ディスク逼迫時の処理」).
//
// Every DiskCheckInterval the job measures the usage of the filesystem that
// holds the recordings directory. At or above DiskHighPercent it deletes
// local segments until the usage is below DiskLowPercent (hysteresis), in
// this order:
//
//  1. Uploaded rows, oldest first, even within the delete grace period:
//     the grace is a safety margin, and when the disk is full the copies
//     that are already in Garage are the right thing to drop first. The
//     file is checked like after the grace period (checkUploaded), with
//     one difference: when only the HEAD request fails (Garage is
//     unreachable), the upload-time record is accepted instead (the local
//     file still matches uploaded_size / uploaded_mtime and the recordings
//     row has the same object key and size). A definitive "missing" or
//     "different size" from Garage still means upload again. Each deleted
//     copy is recorded as a disk_pressure event (uploaded_copy_removed,
//     verified: head or upload_record).
//  2. Rows not uploaded yet, oldest first. Each discarded file is recorded
//     as a disk_pressure event (action segment_discarded).
//
// Only files that have a pending_uploads row are touched (the directory is
// scanned first so complete segments get one), the newest segment of every
// path is never touched (MediaMTX may be writing it), and paths are
// validated with Layout.Resolve. Rows are leased like a worker does, so a
// row a worker is processing (leased_until in the future) is skipped and a
// worker never picks up a row the job holds. Rows the job does not delete
// get their previous next_attempt_at back.
//
// Recording never stops because of this job: errors are logged and the
// next check tries again.

// Event kind and actions of the disk pressure job.
const (
	EventDiskPressure = "disk_pressure"

	diskActionDiscarded       = "segment_discarded"
	diskActionUploadedRemoved = "uploaded_copy_removed"

	// How an uploaded copy was verified before deletion.
	verifiedByHead         = "head"
	verifiedByUploadRecord = "upload_record"

	// Summary actions of a run.
	diskActionRelieved   = "relieved"
	diskActionUnresolved = "unresolved"
)

// Defaults of the disk pressure job.
const (
	DefaultDiskHighPercent   = 90.0
	DefaultDiskLowPercent    = 85.0
	DefaultDiskCheckInterval = 30 * time.Second
	// unresolvedEventEvery limits the unresolved event (nothing could be
	// deleted while above the high watermark).
	unresolvedEventEvery = 10 * time.Minute
	// maxDiskRowsPerRun bounds the rows one run looks at.
	maxDiskRowsPerRun = 100000
)

// DiskUsage is the usage of a filesystem in blocks.
type DiskUsage struct {
	// Used is total minus free blocks (including blocks reserved for root).
	Used uint64
	// Avail is the blocks an unprivileged process may still use.
	Avail uint64
	// BlockSize is the size of a block in bytes.
	BlockSize uint64
}

// NewDiskUsage converts statfs(2) counts: total blocks, free blocks
// (including reserved) and blocks available to unprivileged users.
func NewDiskUsage(blocks, free, avail, blockSize uint64) DiskUsage {
	used := uint64(0)
	if blocks > free {
		used = blocks - free
	}
	return DiskUsage{Used: used, Avail: avail, BlockSize: blockSize}
}

// Percent is the usage as df(1) reports it: used / (used + avail). Blocks
// reserved for root count as neither, so a filesystem that only root can
// still write to is at 100%.
func (u DiskUsage) Percent() float64 {
	if u.Used+u.Avail == 0 {
		return 0
	}
	return float64(u.Used) * 100 / float64(u.Used+u.Avail)
}

// DiskPressureResult reports one run of the disk pressure job.
type DiskPressureResult struct {
	// Before and After are the usage in percent at the start and the end.
	Before, After float64
	// RemovedUploaded counts deleted local copies of uploaded segments.
	RemovedUploaded int
	// Discarded counts deleted segments that were not uploaded.
	Discarded int
	// FreedBytes is the size of the deleted files.
	FreedBytes int64
}

func (s *Service) diskEnabled() bool { return s.o.DiskHighPercent > 0 }

func (s *Service) diskLoop(ctx context.Context) {
	t := time.NewTicker(s.o.DiskCheckInterval)
	defer t.Stop()
	for {
		if _, err := s.RelieveDiskPressure(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("disk pressure check failed; will retry", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) diskUsage() (float64, error) {
	u, err := s.o.StatFS(s.o.Layout.Root())
	if err != nil {
		return 0, fmt.Errorf("ingest: disk usage: %w", err)
	}
	return u.Percent(), nil
}

// RelieveDiskPressure runs the disk pressure job once.
func (s *Service) RelieveDiskPressure(ctx context.Context) (DiskPressureResult, error) {
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	var res DiskPressureResult
	u, err := s.diskUsage()
	if err != nil {
		return res, err
	}
	res.Before, res.After = u, u
	if u < s.o.DiskHighPercent {
		return res, nil
	}
	log := s.log.With(slog.String("job", "disk_pressure"))
	log.Warn("disk usage is above the high watermark; deleting local segments",
		slog.Float64("usage_percent", round1(u)), slog.Float64("high_watermark", s.o.DiskHighPercent),
		slog.Float64("low_watermark", s.o.DiskLowPercent))

	// Give complete segments that were not queued yet a row, so the
	// oldest-first order covers them.
	if n, err := s.Scan(ctx); err != nil {
		log.Warn("recordings scan failed; continuing with the queued segments", slog.String("error", err.Error()))
	} else if n > 0 {
		s.notify()
	}
	protected, err := s.newestSegments()
	if err != nil {
		return res, err
	}

	err = s.relieve(ctx, log, &res, protected)
	s.reportDiskPressure(ctx, log, res)
	return res, err
}

func (s *Service) relieve(ctx context.Context, log *slog.Logger, res *DiskPressureResult, protected []string) error {
	tried := []uuid.UUID{}
	for _, uploaded := range []bool{true, false} {
		for res.After >= s.o.DiskLowPercent && len(tried) < maxDiskRowsPerRun {
			if err := ctx.Err(); err != nil {
				return err
			}
			row, prev, err := s.o.Queue.ClaimForDiskPressure(ctx, uploaded, s.o.Now().Add(s.o.Lease), protected, tried)
			if err != nil {
				return fmt.Errorf("ingest: claim for disk pressure: %w", err)
			}
			if row == nil {
				break
			}
			tried = append(tried, row.ID)
			rlog := log.With(slog.String("pending_upload_id", row.ID.String()), slog.String("local_path", row.LocalPath))
			var size int64
			var removed bool
			if uploaded {
				size, removed = s.dropUploaded(ctx, rlog, *row, prev)
			} else {
				size, removed = s.discard(ctx, rlog, *row, prev)
			}
			if !removed {
				continue
			}
			res.FreedBytes += size
			if uploaded {
				res.RemovedUploaded++
			} else {
				res.Discarded++
			}
			if res.After, err = s.diskUsage(); err != nil {
				return err
			}
		}
	}
	return nil
}

// dropUploaded deletes the local copy of an uploaded segment if it checks
// out. It reports the size of the deleted file.
func (s *Service) dropUploaded(ctx context.Context, log *slog.Logger, row sqlc.PendingUpload, prev time.Time) (int64, bool) {
	seg, err := s.o.Layout.Resolve(row.LocalPath)
	switch {
	case errors.Is(err, ErrSegmentNotFound):
		s.deleteRow(ctx, log, row)
		return 0, false
	case err != nil:
		log.Warn("not deleting an invalid segment path", slog.String("error", err.Error()))
		s.restore(ctx, log, row, prev)
		return 0, false
	}
	verified := verifiedByHead
	reupload, err := s.checkUploaded(ctx, row, seg)
	if errors.Is(err, errStorageUnavailable) {
		// The file matches the upload but Garage cannot be asked. Under
		// pressure, keeping copies that were stored and registered while
		// discarding the only copy of segments not uploaded yet would be
		// worse, so accept the upload-time record instead.
		if ok, rerr := s.uploadRecordMatches(ctx, row, seg); rerr != nil {
			err = errors.Join(err, rerr)
		} else if ok {
			log.Warn("storage cannot be asked; deleting the uploaded copy on the strength of the upload record",
				slog.String("error", err.Error()))
			err, verified = nil, verifiedByUploadRecord
		}
	}
	switch {
	case err != nil:
		// Keep the file and its schedule; the regular flow retries.
		log.Warn("cannot verify the uploaded segment; keeping it", slog.String("error", err.Error()))
		s.restore(ctx, log, row, prev)
		return 0, false
	case reupload != "":
		s.requeue(ctx, log, row, reupload)
		return 0, false
	}
	size := *row.UploadedSize
	if err := os.Remove(seg.LocalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Error("delete local segment failed", slog.String("error", err.Error()))
		s.restore(ctx, log, row, prev)
		return 0, false
	}
	s.deleteRow(ctx, log, row)
	log.Info("deleted the local copy of an uploaded segment because the disk is full",
		slog.Int64("size_bytes", size), slog.String("verified", verified))
	s.event(ctx, log, EventDiskPressure, s.knownCamera(ctx, seg), map[string]any{
		"action":     diskActionUploadedRemoved,
		"verified":   verified,
		"camera_id":  seg.CameraID.String(),
		"local_path": row.LocalPath,
		"object_key": VideoKey(seg.CameraID, seg.StartedAt),
		"size_bytes": size,
		"started_at": seg.StartedAt.Format(time.RFC3339Nano),
	})
	return size, true
}

// uploadRecordMatches reports whether the catalog still has the recording
// registered at upload time with the uploaded size. checkUploaded has
// already compared the local file with uploaded_size / uploaded_mtime.
func (s *Service) uploadRecordMatches(ctx context.Context, row sqlc.PendingUpload, seg Segment) (bool, error) {
	if row.UploadedAt == nil || row.UploadedSize == nil || row.UploadedMtime == nil {
		return false, nil
	}
	size, ok, err := s.o.Queue.RecordingSize(ctx, VideoKey(seg.CameraID, seg.StartedAt))
	if err != nil {
		return false, fmt.Errorf("look up recording: %w", err)
	}
	return ok && size == *row.UploadedSize, nil
}

// knownCamera returns the segment's camera ID if the camera exists
// (events.camera_id references cameras), else nil.
func (s *Service) knownCamera(ctx context.Context, seg Segment) *uuid.UUID {
	if ok, err := s.o.Queue.CameraExists(ctx, seg.CameraID); err == nil && ok {
		id := seg.CameraID
		return &id
	}
	return nil
}

// discard deletes a segment that was not uploaded and records an event.
func (s *Service) discard(ctx context.Context, log *slog.Logger, row sqlc.PendingUpload, prev time.Time) (int64, bool) {
	seg, err := s.o.Layout.Resolve(row.LocalPath)
	switch {
	case errors.Is(err, ErrSegmentNotFound):
		s.deleteRow(ctx, log, row)
		return 0, false
	case err != nil:
		log.Warn("not deleting an invalid segment path", slog.String("error", err.Error()))
		s.restore(ctx, log, row, prev)
		return 0, false
	}
	fi, err := os.Stat(seg.LocalPath)
	if err == nil {
		err = os.Remove(seg.LocalPath)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Error("discard segment failed", slog.String("error", err.Error()))
		s.restore(ctx, log, row, prev)
		return 0, false
	}
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	s.deleteRow(ctx, log, row)
	log.Warn("discarded a segment that was not uploaded because the disk is full",
		slog.Int64("size_bytes", size), slog.Time("started_at", seg.StartedAt))

	// A deleted camera's id goes only into the payload.
	s.event(ctx, log, EventDiskPressure, s.knownCamera(ctx, seg), map[string]any{
		"action":     diskActionDiscarded,
		"camera_id":  seg.CameraID.String(),
		"local_path": row.LocalPath,
		"size_bytes": size,
		"started_at": seg.StartedAt.Format(time.RFC3339Nano),
		"attempts":   row.Attempts,
	})
	return size, true
}

// restore gives a claimed row back with its previous schedule.
func (s *Service) restore(ctx context.Context, log *slog.Logger, row sqlc.PendingUpload, prev time.Time) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	ok, err := s.o.Queue.RestoreSchedule(dctx, row, prev)
	switch {
	case err != nil:
		log.Error("restore pending upload schedule failed", slog.String("error", err.Error()))
	case !ok:
		log.Warn("lost the lease before restoring the schedule")
	}
}

func (s *Service) reportDiskPressure(ctx context.Context, log *slog.Logger, res DiskPressureResult) {
	payload := map[string]any{
		"usage_before":     round1(res.Before),
		"usage_after":      round1(res.After),
		"high_watermark":   s.o.DiskHighPercent,
		"low_watermark":    s.o.DiskLowPercent,
		"removed_uploaded": res.RemovedUploaded,
		"discarded":        res.Discarded,
		"freed_bytes":      res.FreedBytes,
	}
	attrs := []any{
		slog.Float64("usage_before", round1(res.Before)), slog.Float64("usage_after", round1(res.After)),
		slog.Int("removed_uploaded", res.RemovedUploaded), slog.Int("discarded", res.Discarded),
		slog.Int64("freed_bytes", res.FreedBytes),
	}
	if res.RemovedUploaded+res.Discarded > 0 {
		log.Warn("disk pressure handled", attrs...)
		payload["action"] = diskActionRelieved
		s.event(ctx, log, EventDiskPressure, nil, payload)
		return
	}
	if res.After < s.o.DiskHighPercent {
		return
	}
	log.Error("disk usage is above the high watermark and nothing could be deleted", attrs...)
	now := s.o.Now()
	if !s.lastUnresolved.IsZero() && now.Sub(s.lastUnresolved) < unresolvedEventEvery {
		return
	}
	s.lastUnresolved = now
	payload["action"] = diskActionUnresolved
	s.event(ctx, log, EventDiskPressure, nil, payload)
}

// newestSegments returns the newest segment file (by the start time in its
// name) of every cam_<uuid>_main directory, registered camera or not. These
// may still be written by MediaMTX and are never deleted.
func (s *Service) newestSegments() ([]string, error) {
	root := s.o.Layout.Root()
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("ingest: read recordings dir: %w", err)
	}
	out := []string{}
	for _, d := range dirs {
		if !d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			continue
		}
		if _, ok := ParsePathName(d.Name()); !ok {
			continue
		}
		dir := filepath.Join(root, d.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			// Without the listing the newest file is unknown: do nothing.
			return nil, fmt.Errorf("ingest: read %s: %w", dir, err)
		}
		var newest time.Time
		var name string
		for _, e := range entries {
			start, ok := ParseSegmentName(e.Name(), s.o.Layout.location)
			if ok && (name == "" || start.After(newest)) {
				newest, name = start, e.Name()
			}
		}
		if name != "" {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out, nil
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
