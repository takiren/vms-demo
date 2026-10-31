package playback

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
)

func rec(startSec, endSec float64, thumb bool) sqlc.Recording {
	r := sqlc.Recording{
		ID:        uuid.Must(uuid.NewV7()),
		StartedAt: t0.Add(time.Duration(startSec * float64(time.Second))),
		EndedAt:   t0.Add(time.Duration(endSec * float64(time.Second))),
	}
	if thumb {
		k := r.ID.String() + ".jpg"
		r.ThumbnailKey = &k
	}
	return r
}

func at(sec float64) time.Time { return t0.Add(time.Duration(sec * float64(time.Second))) }

func TestMergeIntervals(t *testing.T) {
	recs := []sqlc.Recording{
		rec(-30, 60, false),    // starts before the range: clipped
		rec(60, 120, false),    // touches
		rec(121.5, 180, false), // 1.5 s gap: merged
		rec(150, 170, false),   // inside the previous one
		rec(183, 240, false),   // 3 s gap: new interval
		rec(250, 250, false),   // empty
		rec(300, 400, false),   // ends after the range: clipped
	}
	got := MergeIntervals(recs, at(0), at(350), MergeTolerance)
	want := []Interval{{at(0), at(180)}, {at(183), at(240)}, {at(300), at(350)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeIntervals =\n%v\nwant\n%v", got, want)
	}
	if got := MergeIntervals(nil, at(0), at(1), MergeTolerance); got == nil || len(got) != 0 {
		t.Errorf("empty = %#v, want an empty slice", got)
	}
}

func TestSampleThumbnails(t *testing.T) {
	// One-minute recordings over an hour, every one with a thumbnail.
	var recs []sqlc.Recording
	for i := range 60 {
		recs = append(recs, rec(float64(i*60), float64(i*60+60), true))
	}
	got := SampleThumbnails(recs, at(0), at(3600), 4)
	// Buckets of 15 min; middles at 7.5, 22.5, 37.5, 52.5 min. Ties go to the earlier start.
	var starts []float64
	for _, r := range got {
		starts = append(starts, r.StartedAt.Sub(t0).Minutes())
	}
	if !reflect.DeepEqual(starts, []float64{7, 22, 37, 52}) {
		t.Errorf("starts = %v", starts)
	}
	// More buckets than recordings: each recording once.
	if got := SampleThumbnails(recs[:3], at(0), at(180), 50); len(got) != 3 {
		t.Errorf("got %d thumbnails, want 3", len(got))
	}
	if got := SampleThumbnails(recs, at(0), at(3600), 0); len(got) != 0 {
		t.Errorf("n=0: %d", len(got))
	}
	if got := SampleThumbnails(recs, at(10), at(10), 5); len(got) != 0 {
		t.Errorf("empty range: %d", len(got))
	}
}

func TestSampleThumbnailsSkipsMissingAndKeepsOrder(t *testing.T) {
	long := rec(-100, 1000, true) // overlaps every bucket
	noThumb := rec(240, 260, false)
	near := rec(230, 270, true)
	recs := []sqlc.Recording{long, near, noThumb}
	got := SampleThumbnails(recs, at(0), at(500), 2)
	// Bucket 1 [0,250) mid 125: near (230) is closer than long (-100).
	// Bucket 2 [250,500): near is used, so long. Output is in time order.
	if len(got) != 2 || got[0].ID != long.ID || got[1].ID != near.ID {
		t.Errorf("got %v", got)
	}
	// Recordings outside the range are never picked.
	if got := SampleThumbnails([]sqlc.Recording{rec(0, 10, true)}, at(10), at(20), 1); len(got) != 0 {
		t.Errorf("outside: %v", got)
	}
}

func TestValidateRange(t *testing.T) {
	for _, tc := range []struct {
		from, to time.Time
		ok       bool
	}{
		{at(0), at(1), true},
		{at(0), at(86400), true},
		{at(0), at(86401), false},
		{at(1), at(1), false},
		{at(2), at(1), false},
		{time.Time{}, at(1), false},
	} {
		if err := ValidateRange(tc.from, tc.to, 24*time.Hour); (err == nil) != tc.ok {
			t.Errorf("ValidateRange(%s, %s) = %v", tc.from, tc.to, err)
		}
	}
}

func TestURLTTL(t *testing.T) {
	for _, tc := range []struct{ d, max, want time.Duration }{
		{time.Minute, 12 * time.Hour, time.Hour + time.Minute},
		{0, 12 * time.Hour, time.Hour},
		{24 * time.Hour, 12 * time.Hour, 12 * time.Hour},
		{time.Minute, 30 * time.Minute, 30 * time.Minute},
	} {
		if got := URLTTL(tc.d, tc.max); got != tc.want {
			t.Errorf("URLTTL(%s, %s) = %s, want %s", tc.d, tc.max, got, tc.want)
		}
	}
}
