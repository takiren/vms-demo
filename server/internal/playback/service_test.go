package playback

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
)

var testCam = uuid.MustParse("01900000-0000-7000-8000-0000000000c1")

type fakeCatalog struct {
	mu      sync.Mutex
	recs    []sqlc.Recording
	limit   int32
	cached  map[uuid.UUID]int64
	listErr error
}

func (f *fakeCatalog) CameraExists(_ context.Context, id uuid.UUID) (bool, error) {
	return id == testCam, nil
}

func (f *fakeCatalog) ListRecordings(_ context.Context, _ uuid.UUID, from, to time.Time, limit int32) ([]sqlc.Recording, error) {
	f.limit = limit
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []sqlc.Recording
	for _, r := range f.recs {
		if r.StartedAt.Before(to) && r.EndedAt.After(from) && len(out) < int(limit) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeCatalog) SetInitSize(_ context.Context, id uuid.UUID, _, n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cached == nil {
		f.cached = map[uuid.UUID]int64{}
	}
	f.cached[id] = n
	return nil
}

type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	gets    []int64 // requested lengths
	ttls    []time.Duration
	getErr  error
}

func (f *fakeStore) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ttls = append(f.ttls, ttl)
	return "https://s3.vms.example/recordings/" + key + "?sig=x", nil
}

func (f *fakeStore) GetRange(_ context.Context, key string, off, length int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, length)
	if f.getErr != nil {
		return nil, f.getErr
	}
	b, ok := f.objects[key]
	if !ok {
		return nil, errors.New("no such key")
	}
	return b[off:min(off+length, int64(len(b)))], nil
}

func newTestService(t *testing.T, cat *fakeCatalog, st *fakeStore) *Service {
	t.Helper()
	s, err := New(Options{Catalog: cat, Store: st, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func withInit(r sqlc.Recording, key string, init, size int64) sqlc.Recording {
	r.CameraID, r.ObjectKey, r.SizeBytes = testCam, key, size
	if init > 0 {
		r.InitSize = &init
	}
	return r
}

func TestNewRequiresDeps(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New without deps succeeded")
	}
}

func TestRecordingsInRangeErrors(t *testing.T) {
	cat := &fakeCatalog{}
	s := newTestService(t, cat, &fakeStore{})
	ctx := t.Context()
	if _, err := s.RecordingsInRange(ctx, testCam, at(10), at(0)); !errors.Is(err, ErrInvalidRange) {
		t.Errorf("reversed: %v", err)
	}
	if _, err := s.RecordingsInRange(ctx, testCam, at(0), at(0).Add(25*time.Hour)); !errors.Is(err, ErrInvalidRange) {
		t.Errorf("too long: %v", err)
	}
	if _, err := s.RecordingsInRange(ctx, uuid.New(), at(0), at(10)); !errors.Is(err, ErrCameraNotFound) {
		t.Errorf("unknown camera: %v", err)
	}
	for i := range MaxRecordings + 1 {
		cat.recs = append(cat.recs, rec(float64(i), float64(i)+1, false))
	}
	if _, err := s.RecordingsInRange(ctx, testCam, at(0), at(86400)); !errors.Is(err, ErrTooManyRecordings) {
		t.Errorf("too many: %v", err)
	}
	if cat.limit != MaxRecordings+1 {
		t.Errorf("limit = %d", cat.limit)
	}
	cat.listErr = errors.New("db down")
	if _, err := s.RecordingsInRange(ctx, testCam, at(0), at(10)); err == nil || errors.Is(err, ErrTooManyRecordings) {
		t.Errorf("db error: %v", err)
	}
}

func TestPlaylist(t *testing.T) {
	seg, err := os.ReadFile("../../../docker/ffmpeg/testdata/h264_aac_0.mp4")
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(seg))
	legacy := withInit(rec(60, 120, false), "legacy.mp4", 0, size)
	broken := withInit(rec(120, 180, false), "broken.mp4", 0, 100)
	cat := &fakeCatalog{recs: []sqlc.Recording{
		withInit(rec(-10, 60, false), "a.mp4", 1219, size),
		legacy,
		broken,
		withInit(rec(200, 200, false), "empty.mp4", 1219, size),
	}}
	st := &fakeStore{objects: map[string][]byte{"legacy.mp4": seg, "broken.mp4": []byte(strings.Repeat("x", 100))}}
	s := newTestService(t, cat, st)
	pl, err := s.Playlist(t.Context(), testCam, at(0), at(3600))
	if err != nil {
		t.Fatal(err)
	}
	text := string(pl)
	for _, want := range []string{
		"#EXT-X-START:TIME-OFFSET=10.000,PRECISE=YES\n",
		`#EXT-X-MAP:URI="https://s3.vms.example/recordings/a.mp4?sig=x",BYTERANGE="1219@0"`,
		`#EXT-X-MAP:URI="https://s3.vms.example/recordings/legacy.mp4?sig=x",BYTERANGE="1219@0"`,
		"#EXT-X-PROGRAM-DATE-TIME:2026-01-02T02:59:50.000Z\n",
		"#EXTINF:70.000,\n",
		"#EXT-X-TARGETDURATION:70\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "broken.mp4") || strings.Contains(text, "empty.mp4") || strings.Count(text, "#EXT-X-DISCONTINUITY") != 1 {
		t.Errorf("unplayable recordings included:\n%s", text)
	}
	if cat.cached[legacy.ID] != 1219 {
		t.Errorf("init size not cached: %v", cat.cached)
	}
	if _, ok := cat.cached[broken.ID]; ok {
		t.Error("broken recording cached")
	}
	// 1 h range + 1 h margin.
	for _, ttl := range st.ttls {
		if ttl != 2*time.Hour {
			t.Errorf("ttl = %s", ttl)
		}
	}
}

func TestPlaylistReadsMoreForLargeInit(t *testing.T) {
	// moov larger than the first probe.
	big := make([]byte, initProbeBytes+100)
	copy(big, box("ftyp", 4))
	copy(big[12:], box("moov", initProbeBytes))
	copy(big[12+8+initProbeBytes:], box("moof", 0))
	cat := &fakeCatalog{recs: []sqlc.Recording{withInit(rec(0, 60, false), "big.mp4", 0, int64(len(big)))}}
	st := &fakeStore{objects: map[string][]byte{"big.mp4": big}}
	s := newTestService(t, cat, st)
	if _, err := s.Playlist(t.Context(), testCam, at(0), at(60)); err != nil {
		t.Fatal(err)
	}
	if len(st.gets) != 2 || st.gets[0] != initProbeBytes || st.gets[1] != int64(len(big)) {
		t.Errorf("gets = %v", st.gets)
	}
	if cat.cached[cat.recs[0].ID] != 12+8+initProbeBytes {
		t.Errorf("cached = %v", cat.cached)
	}
}

func TestPlaylistErrors(t *testing.T) {
	ctx := t.Context()
	s := newTestService(t, &fakeCatalog{}, &fakeStore{})
	if _, err := s.Playlist(ctx, testCam, at(0), at(60)); !errors.Is(err, ErrNoRecordings) {
		t.Errorf("no recordings: %v", err)
	}
	if _, err := s.Playlist(ctx, testCam, at(60), at(0)); !errors.Is(err, ErrInvalidRange) {
		t.Errorf("invalid range: %v", err)
	}
	cat := &fakeCatalog{recs: []sqlc.Recording{withInit(rec(0, 60, false), "a.mp4", 0, 5000)}}
	s = newTestService(t, cat, &fakeStore{getErr: errors.New("connection refused")})
	if _, err := s.Playlist(ctx, testCam, at(0), at(60)); !errors.Is(err, ErrStorage) {
		t.Errorf("storage down: %v", err)
	}
}

func TestTimeline(t *testing.T) {
	cat := &fakeCatalog{recs: []sqlc.Recording{
		withInit(rec(0, 60, true), "a.mp4", 1, 2),
		withInit(rec(61, 120, true), "b.mp4", 1, 2),
		withInit(rec(600, 660, false), "c.mp4", 1, 2),
	}}
	st := &fakeStore{}
	s := newTestService(t, cat, st)
	tl, err := s.Timeline(t.Context(), testCam, at(0), at(3600), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Intervals) != 2 || !tl.Intervals[0].End.Equal(at(120)) || !tl.Intervals[1].Start.Equal(at(600)) {
		t.Errorf("intervals = %v", tl.Intervals)
	}
	if len(tl.Thumbnails) != 2 || tl.Thumbnails[0].RecordingID != cat.recs[0].ID ||
		!strings.HasSuffix(strings.Split(tl.Thumbnails[0].URL, "?")[0], cat.recs[0].ID.String()+".jpg") {
		t.Errorf("thumbnails = %+v", tl.Thumbnails)
	}
	if !tl.URLsExpireAt.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("expires = %s", tl.URLsExpireAt)
	}
	tl, err = s.Timeline(t.Context(), testCam, at(0), at(3600), -1)
	if err != nil || len(tl.Thumbnails) != 0 || tl.Thumbnails == nil {
		t.Errorf("no thumbnails: %+v, %v", tl.Thumbnails, err)
	}
	if _, err := s.Timeline(t.Context(), uuid.New(), at(0), at(60), 1); !errors.Is(err, ErrCameraNotFound) {
		t.Errorf("unknown camera: %v", err)
	}
}
