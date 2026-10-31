package ingest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Errors from Layout.
var (
	// ErrInvalidSegment means the path is not a MediaMTX recording segment
	// inside the recordings directory. It wraps a reason.
	ErrInvalidSegment = errors.New("invalid segment path")
	// ErrSegmentNotFound means the segment file does not exist (any more).
	ErrSegmentNotFound = errors.New("segment file not found")
)

var (
	// MediaMTX records only the main path of each camera (cam_<uuid>_main).
	pathNamePattern = regexp.MustCompile(`^cam_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})_main$`)
	// recordPath %Y-%m-%d_%H-%M-%S-%f (microseconds) plus the fMP4 extension.
	segmentNamePattern = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})-([0-9]{2})_([0-9]{2})-([0-9]{2})-([0-9]{2})-([0-9]{6})\.mp4$`)
)

// Segment is a validated recording segment.
type Segment struct {
	CameraID uuid.UUID
	// LocalPath is the absolute path of the file as the API sees it.
	LocalPath string
	// StartedAt is the segment start from its file name, in UTC.
	StartedAt time.Time
}

// Layout maps MediaMTX's view of the recordings directory to the API's and
// validates segment paths. The API and MediaMTX may mount the directory at
// different places (deploy/data/recordings on the host and /recordings in
// the container during development), so paths are mapped by their part
// relative to the directory.
type Layout struct {
	root     string // the API's recordings directory, symlinks resolved
	mtxRoot  string // MediaMTX's recordings directory (path syntax)
	location *time.Location
}

// NewLayout checks that localDir is an existing directory. mtxDir is the
// directory MediaMTX writes to (its recordPath prefix). loc is the zone
// MediaMTX formats file names in (nil means UTC).
func NewLayout(localDir, mtxDir string, loc *time.Location) (*Layout, error) {
	if loc == nil {
		loc = time.UTC
	}
	abs, err := filepath.Abs(localDir)
	if err != nil {
		return nil, fmt.Errorf("ingest: recordings dir: %w", err)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("ingest: recordings dir: %w", err)
	}
	fi, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("ingest: recordings dir: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("ingest: recordings dir %s is not a directory", root)
	}
	mtx := path.Clean(mtxDir)
	if !path.IsAbs(mtx) || mtx == "/" {
		return nil, fmt.Errorf("ingest: MediaMTX recordings dir %q must be an absolute directory other than /", mtxDir)
	}
	return &Layout{root: root, mtxRoot: mtx, location: loc}, nil
}

// Root returns the API's recordings directory (symlinks resolved).
func (l *Layout) Root() string { return l.root }

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSegment, fmt.Sprintf(format, args...))
}

// FromHook validates a segment-complete notification: segmentPath must be
// <MediaMTX dir>/<mtxPath>/<file>, mtxPath must be cam_<uuid>_main, and
// the file must exist in the API's directory as a regular file reached
// without symlinks.
func (l *Layout) FromHook(mtxPath, segmentPath string) (Segment, error) {
	if strings.ContainsRune(segmentPath, 0) || strings.ContainsRune(mtxPath, 0) {
		return Segment{}, invalid("NUL byte")
	}
	if !path.IsAbs(segmentPath) {
		return Segment{}, invalid("segment_path must be absolute")
	}
	for elem := range strings.SplitSeq(segmentPath, "/") {
		if elem == ".." || elem == "." {
			return Segment{}, invalid("segment_path must not contain . or .. elements")
		}
	}
	if path.Clean(segmentPath) != segmentPath {
		return Segment{}, invalid("segment_path is not clean")
	}
	rel, ok := strings.CutPrefix(segmentPath, l.mtxRoot+"/")
	if !ok {
		return Segment{}, invalid("segment_path is outside the recordings directory")
	}
	dir, _, _ := strings.Cut(rel, "/")
	if dir != mtxPath {
		return Segment{}, invalid("segment_path is not in the directory of path")
	}
	return l.resolveRel(rel)
}

// Resolve validates a local path (from pending_uploads or the directory
// scan) the same way as FromHook.
func (l *Layout) Resolve(localPath string) (Segment, error) {
	if strings.ContainsRune(localPath, 0) {
		return Segment{}, invalid("NUL byte")
	}
	if !filepath.IsAbs(localPath) || filepath.Clean(localPath) != localPath {
		return Segment{}, invalid("local path must be absolute and clean")
	}
	rel, ok := strings.CutPrefix(localPath, l.root+string(filepath.Separator))
	if !ok {
		return Segment{}, invalid("local path is outside the recordings directory")
	}
	return l.resolveRel(filepath.ToSlash(rel))
}

// resolveRel parses <cam_<uuid>_main>/<timestamp>.mp4 (slash separated) and
// checks the file.
func (l *Layout) resolveRel(rel string) (Segment, error) {
	parts := strings.Split(rel, "/")
	if len(parts) != 2 {
		return Segment{}, invalid("segment must be <path>/<file> directly under the recordings directory")
	}
	camID, ok := ParsePathName(parts[0])
	if !ok {
		return Segment{}, invalid("path must be cam_<uuid>_main")
	}
	start, ok := ParseSegmentName(parts[1], l.location)
	if !ok {
		return Segment{}, invalid("file name must be YYYY-MM-DD_HH-MM-SS-ffffff.mp4")
	}
	local := filepath.Join(l.root, parts[0], parts[1])
	if err := checkRegularNoSymlink(local); err != nil {
		return Segment{}, err
	}
	return Segment{CameraID: camID, LocalPath: local, StartedAt: start}, nil
}

// checkRegularNoSymlink requires local to be a regular file whose path
// contains no symlinks (root itself is already resolved, so any symlink in
// the last two elements changes the resolved path).
func checkRegularNoSymlink(local string) error {
	resolved, err := filepath.EvalSymlinks(local)
	if errors.Is(err, fs.ErrNotExist) {
		// A dangling symlink is also reported here; Lstat tells them apart.
		if fi, lerr := os.Lstat(local); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return invalid("segment must not be a symlink")
		}
		return ErrSegmentNotFound
	}
	if err != nil {
		return fmt.Errorf("ingest: resolve %s: %w", local, err)
	}
	if resolved != local {
		return invalid("segment path must not go through symlinks")
	}
	fi, err := os.Lstat(local)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrSegmentNotFound
	}
	if err != nil {
		return fmt.Errorf("ingest: stat %s: %w", local, err)
	}
	if !fi.Mode().IsRegular() {
		return invalid("segment must be a regular file")
	}
	return nil
}

// ParsePathName returns the camera ID of a MediaMTX path name
// cam_<uuid>_main (lower-case UUID, as the API creates them).
func ParsePathName(name string) (uuid.UUID, bool) {
	m := pathNamePattern.FindStringSubmatch(name)
	if m == nil {
		return uuid.UUID{}, false
	}
	id, err := uuid.Parse(m[1])
	if err != nil {
		return uuid.UUID{}, false
	}
	return id, true
}

// ParseSegmentName parses a MediaMTX segment file name
// (%Y-%m-%d_%H-%M-%S-%f.mp4). MediaMTX formats it in its own local time
// zone (the container's TZ, UTC unless set), so the caller passes that
// zone. The result is in UTC.
func ParseSegmentName(name string, loc *time.Location) (time.Time, bool) {
	m := segmentNamePattern.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	var n [7]int
	for i := range n {
		v, err := strconv.Atoi(m[i+1])
		if err != nil {
			return time.Time{}, false
		}
		n[i] = v
	}
	t := time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], n[5], n[6]*1000, loc)
	// time.Date normalizes out-of-range values (Feb 30, 25:00); reject them.
	if t.Year() != n[0] || int(t.Month()) != n[1] || t.Day() != n[2] ||
		t.Hour() != n[3] || t.Minute() != n[4] || t.Second() != n[5] {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// VideoKey is the object key of a segment:
// <camera_id>/<YYYY>/<MM>/<DD>/<HH>/<started_at_unix_ms>.mp4, in UTC.
func VideoKey(cameraID uuid.UUID, startedAt time.Time) string {
	u := startedAt.UTC()
	return fmt.Sprintf("%s/%04d/%02d/%02d/%02d/%d.mp4",
		cameraID, u.Year(), int(u.Month()), u.Day(), u.Hour(), u.UnixMilli())
}

// ThumbnailKey is the key of a segment's thumbnail: the video key with .jpg.
func ThumbnailKey(videoKey string) string {
	return strings.TrimSuffix(videoKey, ".mp4") + ".jpg"
}
