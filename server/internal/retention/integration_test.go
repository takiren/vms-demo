package retention

import (
	"bytes"
	"context"
	"flag"
	"log"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
	"github.com/takiren/vms-demo/server/internal/s3store"
	"github.com/takiren/vms-demo/server/internal/testutil/garagetest"
	"github.com/takiren/vms-demo/server/internal/testutil/pgtest"
)

func TestMain(m *testing.M) {
	flag.Parse()
	if err := garagetest.Start(context.Background()); err != nil {
		log.Fatal(err)
	}
	code := pgtest.Run(m)
	garagetest.Stop()
	os.Exit(code)
}

// partialStore makes keys in fail fail, like a per-key DeleteObjects error.
type partialStore struct {
	*s3store.Store
	fail map[string]bool
}

func (p partialStore) Delete(ctx context.Context, keys []string) (s3store.DeleteResult, error) {
	var pass []string
	for _, k := range keys {
		if !p.fail[k] {
			pass = append(pass, k)
		}
	}
	res, err := p.Store.Delete(ctx, pass)
	for k := range p.fail {
		res.Failed[k] = context.DeadlineExceeded
	}
	return res, err
}

// TestRetentionEndToEnd runs the job against PostgreSQL and Garage.
func TestRetentionEndToEnd(t *testing.T) {
	pool := pgtest.NewPool(t)
	store, err := s3store.New(garagetest.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	q := sqlc.New(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	short, err := q.CreateCamera(ctx, sqlc.CreateCameraParams{Name: "short", MainUrl: "rtsp://a/main", RetentionDays: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	long, err := q.CreateCamera(ctx, sqlc.CreateCameraParams{Name: "long", MainUrl: "rtsp://b/main", RetentionDays: 30, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	put := func(key string) {
		if err := store.Put(ctx, key, bytes.NewReader([]byte("x")), 1, "video/mp4"); err != nil {
			t.Fatal(err)
		}
	}
	type rec struct {
		cam   uuid.UUID
		start time.Time
		dur   time.Duration
		thumb bool
	}
	recs := map[string]rec{
		"expired":         {short.ID, now.Add(-50 * time.Hour), time.Minute, true},
		"expired-nothumb": {short.ID, now.Add(-49 * time.Hour), time.Minute, false},
		"expired-thumbko": {short.ID, now.Add(-48 * time.Hour), time.Minute, true},
		// Ends inside the retention period: kept although it started before.
		"straddles": {short.ID, now.Add(-24*time.Hour - time.Minute), 2 * time.Minute, true},
		"recent":    {short.ID, now.Add(-time.Hour), time.Minute, true},
		"long-cam":  {long.ID, now.Add(-50 * time.Hour), time.Minute, true},
	}
	ids := map[string]uuid.UUID{}
	for name, r := range recs {
		key := r.cam.String() + "/" + name + ".mp4"
		put(key)
		var thumb *string
		if r.thumb {
			k := r.cam.String() + "/" + name + ".jpg"
			put(k)
			thumb = &k
		}
		row, err := q.InsertRecording(ctx, sqlc.InsertRecordingParams{
			CameraID: r.cam, StartedAt: r.start, EndedAt: r.start.Add(r.dur), ObjectKey: key, SizeBytes: 1, ThumbnailKey: thumb,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = row.ID
	}
	// A recording whose objects were already deleted by an interrupted run.
	gone, err := q.InsertRecording(ctx, sqlc.InsertRecordingParams{
		CameraID: short.ID, StartedAt: now.Add(-60 * time.Hour), EndedAt: now.Add(-60 * time.Hour), ObjectKey: short.ID.String() + "/gone.mp4", SizeBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids["gone"] = gone.ID

	thumbKO := short.ID.String() + "/expired-thumbko.jpg"
	j, err := New(Options{
		Catalog: NewPGCatalog(pool), Store: partialStore{store, map[string]bool{thumbKO: true}},
		BatchSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Another process holds the lock: nothing happens.
	other, ok, err := NewPGCatalog(pool).TryLock(ctx)
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	if res, err := j.RunOnce(ctx); err != nil || !res.Skipped {
		t.Fatalf("RunOnce while locked = %+v, %v", res, err)
	}
	other()

	res, err := j.RunOnce(ctx)
	if err != nil || res.Deleted != 3 || res.Failed != 1 {
		t.Fatalf("RunOnce = %+v, %v", res, err)
	}
	exists := func(name string) bool {
		_, err := q.GetRecording(ctx, ids[name])
		return err == nil
	}
	for name, want := range map[string]bool{
		"gone": false, "expired": false, "expired-nothumb": false,
		"expired-thumbko": true, "straddles": true, "recent": true, "long-cam": true,
	} {
		if exists(name) != want {
			t.Errorf("recording %s exists = %v, want %v", name, !want, want)
		}
	}
	for key, want := range map[string]bool{
		short.ID.String() + "/expired.mp4": false, short.ID.String() + "/expired.jpg": false,
		short.ID.String() + "/expired-nothumb.mp4": false,
		short.ID.String() + "/expired-thumbko.mp4": false, thumbKO: true,
		short.ID.String() + "/straddles.mp4": true, long.ID.String() + "/long-cam.mp4": true,
	} {
		if _, ok, err := store.Head(ctx, key); err != nil || ok != want {
			t.Errorf("object %s exists = %v (%v), want %v", key, ok, err, want)
		}
	}
	evs, err := q.ListRecentEvents(ctx, 10)
	if err != nil || len(evs) != 1 || evs[0].Kind != EventDeleteFailed || !bytes.Contains(evs[0].Payload, []byte(thumbKO)) {
		t.Fatalf("events = %+v, %v", evs, err)
	}

	// Next run, the thumbnail deletes; the video is already gone (reported
	// deleted), so the row goes.
	j2, _ := New(Options{Catalog: NewPGCatalog(pool), Store: store})
	if res, err := j2.RunOnce(ctx); err != nil || res.Deleted != 1 || res.Failed != 0 {
		t.Fatalf("second RunOnce = %+v, %v", res, err)
	}
	if exists("expired-thumbko") {
		t.Error("recording kept after its objects were deleted")
	}
	// The lock was released.
	unlock, ok, err := NewPGCatalog(pool).TryLock(ctx)
	if err != nil || !ok {
		t.Fatalf("lock not released: %v, %v", ok, err)
	}
	unlock()
}
