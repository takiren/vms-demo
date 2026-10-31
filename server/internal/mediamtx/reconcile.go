package mediamtx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ManagedPrefix is the prefix of every path the API manages. Paths without
// it are never touched.
const ManagedPrefix = "cam_"

// MainPath is the recording (and single-view live) path of a camera.
func MainPath(id uuid.UUID) string { return ManagedPrefix + id.String() + "_main" }

// SubPath is the multi-view live path of a camera (its H.264 sub stream).
func SubPath(id uuid.UUID) string { return ManagedPrefix + id.String() + "_sub" }

// Camera is what the reconciler needs to know about one enabled camera.
type Camera struct {
	ID uuid.UUID
	// MainSource is the main stream URL with credentials injected.
	MainSource string
	// SubSource is the sub stream URL with credentials injected, or "" if
	// the camera has no sub stream.
	SubSource string
	// Unavailable means the sources could not be built (for example the
	// credentials cannot be decrypted). The camera's paths are then left as
	// they are in MediaMTX, neither added, changed nor removed, so a
	// transient problem does not stop a running recording.
	Unavailable bool
}

// CameraSource lists the enabled cameras. Disabled cameras must not be
// returned: their paths are removed.
type CameraSource interface {
	EnabledCameraSources(ctx context.Context) ([]Camera, error)
}

// Desired returns the path configs the cameras should have, and the names
// to leave alone (cameras that are Unavailable).
//
//   - _main: record: true, always connected (sourceOnDemand: false), since
//     recording is continuous.
//   - _sub: record: false, connected only while someone watches
//     (sourceOnDemand: true), to save camera and network bandwidth.
func Desired(cams []Camera) (map[string]PathConf, map[string]bool) {
	want := map[string]PathConf{}
	keep := map[string]bool{}
	for _, c := range cams {
		mainName, subName := MainPath(c.ID), SubPath(c.ID)
		if c.Unavailable {
			keep[mainName], keep[subName] = true, true
			continue
		}
		want[mainName] = PathConf{Name: mainName, Source: c.MainSource, SourceOnDemand: false, Record: true}
		if c.SubSource != "" {
			want[subName] = PathConf{Name: subName, Source: c.SubSource, SourceOnDemand: true, Record: false}
		}
	}
	return want, keep
}

// OpKind is what to do with one path.
type OpKind string

// The operations Plan produces.
const (
	OpAdd     OpKind = "add"
	OpReplace OpKind = "replace"
	OpDelete  OpKind = "delete"
)

// Op is one change to MediaMTX.
type Op struct {
	Kind OpKind
	Name string
	Conf PathConf // for add and replace
}

// Plan compares the desired paths with the current ones and returns the
// changes, sorted by name. Only paths with ManagedPrefix are considered;
// names in keep are not changed.
func Plan(want map[string]PathConf, keep map[string]bool, current []PathConf) []Op {
	var ops []Op
	have := map[string]PathConf{}
	for _, p := range current {
		if !strings.HasPrefix(p.Name, ManagedPrefix) {
			continue
		}
		have[p.Name] = p
		if _, ok := want[p.Name]; !ok && !keep[p.Name] {
			ops = append(ops, Op{Kind: OpDelete, Name: p.Name})
		}
	}
	for name, w := range want {
		if keep[name] {
			continue
		}
		h, ok := have[name]
		switch {
		case !ok:
			ops = append(ops, Op{Kind: OpAdd, Name: name, Conf: w})
		case h != w:
			ops = append(ops, Op{Kind: OpReplace, Name: name, Conf: w})
		}
	}
	slices.SortFunc(ops, func(a, b Op) int { return strings.Compare(a.Name, b.Name) })
	return ops
}

// API is the part of Client the reconciler uses.
type API interface {
	ListPaths(ctx context.Context) ([]PathConf, error)
	AddPath(ctx context.Context, p PathConf) error
	ReplacePath(ctx context.Context, p PathConf) error
	DeletePath(ctx context.Context, name string) error
}

var _ API = (*Client)(nil)

// Defaults for the reconciler.
const (
	DefaultInterval = 30 * time.Second
	minBackoff      = time.Second
)

// Reconciler applies the cameras in the database to MediaMTX. Run it once in
// the background; call Trigger after every camera change.
type Reconciler struct {
	api      API
	cameras  CameraSource
	logger   *slog.Logger
	interval time.Duration
	trigger  chan struct{}
	mu       sync.Mutex // single flight: one Sync at a time
}

// NewReconciler returns a Reconciler that also syncs every interval (MediaMTX
// forgets paths added through the Control API when it restarts). A zero
// interval means DefaultInterval.
func NewReconciler(api API, cameras CameraSource, logger *slog.Logger, interval time.Duration) *Reconciler {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Reconciler{
		api:      api,
		cameras:  cameras,
		logger:   logger,
		interval: interval,
		trigger:  make(chan struct{}, 1),
	}
}

// Trigger asks for a sync soon. It never blocks; triggers that arrive while
// one is pending are merged.
func (r *Reconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Run syncs immediately, then on every Trigger and every interval, until ctx
// is done. After a failure it retries with exponential backoff (from 1s, at
// most the interval). It never blocks the caller's startup: run it in a
// goroutine.
func (r *Reconciler) Run(ctx context.Context) {
	backoff := minBackoff
	failing := false
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-r.trigger:
		}
		next := r.interval
		if _, err := r.Sync(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			// Log the first failure loudly and repeats quietly, so a MediaMTX
			// outage does not flood the log.
			level := slog.LevelWarn
			if failing {
				level = slog.LevelDebug
			}
			r.logger.Log(ctx, level, "mediamtx sync failed; retrying",
				slog.String("error", Redact(err.Error())), slog.Duration("retry_in", backoff))
			failing = true
			next = backoff
			backoff = min(backoff*2, r.interval)
		} else {
			if failing {
				r.logger.Info("mediamtx sync recovered")
			}
			failing = false
			backoff = minBackoff
		}
		// Since Go 1.23, Reset discards a pending tick; no drain needed.
		timer.Reset(next)
	}
}

// Sync applies the desired state once and returns the operations it
// attempted. Concurrent calls run one after another. A failed operation does
// not stop the others; all failures are returned joined.
func (r *Reconciler) Sync(ctx context.Context) ([]Op, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cams, err := r.cameras.EnabledCameraSources(ctx)
	if err != nil {
		return nil, fmt.Errorf("list cameras: %w", err)
	}
	current, err := r.api.ListPaths(ctx)
	if err != nil {
		return nil, fmt.Errorf("list mediamtx paths: %w", err)
	}
	want, keep := Desired(cams)
	ops := Plan(want, keep, current)

	var errs []error
	for _, op := range ops {
		var err error
		switch op.Kind {
		case OpAdd:
			err = r.api.AddPath(ctx, op.Conf)
		case OpReplace:
			err = r.api.ReplacePath(ctx, op.Conf)
		case OpDelete:
			err = r.api.DeletePath(ctx, op.Name)
			if errors.Is(err, ErrNotFound) {
				err = nil // already gone
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", op.Kind, op.Name, err))
			continue
		}
		r.logger.Info("mediamtx path synced", slog.String("op", string(op.Kind)), slog.String("path", op.Name))
	}
	for name := range keep {
		r.logger.Warn("camera sources unavailable; leaving its mediamtx path unchanged", slog.String("path", name))
	}
	return ops, errors.Join(errs...)
}
