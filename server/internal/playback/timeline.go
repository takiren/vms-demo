package playback

import (
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/takiren/vms-demo/server/internal/db/sqlc"
)

// MergeTolerance is the largest gap: recordings whose gap is at most this are one interval on
// the timeline (MediaMTX's segments of a continuous recording touch, give
// or take rounding).
const MergeTolerance = 2 * time.Second

// Interval is a span of time with recordings.
type Interval struct {
	Start, End time.Time
}

// MergeIntervals returns the spans covered by recs (sorted by StartedAt),
// clipped to [from, to), merging recordings whose gap is at most tolerance.
func MergeIntervals(recs []sqlc.Recording, from, to time.Time, tolerance time.Duration) []Interval {
	out := []Interval{}
	for _, r := range recs {
		start, end := maxTime(r.StartedAt, from), minTime(r.EndedAt, to)
		if !start.Before(end) {
			continue
		}
		if n := len(out); n > 0 && !start.After(out[n-1].End.Add(tolerance)) {
			out[n-1].End = maxTime(out[n-1].End, end)
			continue
		}
		out = append(out, Interval{Start: start, End: end})
	}
	return out
}

// SampleThumbnails picks at most n recordings with a thumbnail, spread
// evenly over [from, to): the range is split into n equal buckets and, for
// each bucket, the recording overlapping it whose start (the thumbnail's
// time) is closest to the bucket's middle is chosen. A recording is used
// at most once. The result is in time order.
func SampleThumbnails(recs []sqlc.Recording, from, to time.Time, n int) []sqlc.Recording {
	out := []sqlc.Recording{}
	if n <= 0 || !from.Before(to) {
		return out
	}
	width := to.Sub(from) / time.Duration(n)
	if width <= 0 {
		width = 1
	}
	used := map[uuid.UUID]bool{}
	first := 0 // recs before this end before the current bucket
	for i := range n {
		bs := from.Add(width * time.Duration(i))
		be := bs.Add(width)
		if i == n-1 {
			be = to
		}
		mid := bs.Add(be.Sub(bs) / 2)
		best := -1
		var bestDist time.Duration
		for j := first; j < len(recs); j++ {
			r := recs[j]
			if !r.StartedAt.Before(be) {
				break // sorted by start: no later recording overlaps
			}
			if !r.EndedAt.After(bs) {
				if j == first {
					first++
				}
				continue
			}
			if r.ThumbnailKey == nil || used[r.ID] {
				continue
			}
			if d := r.StartedAt.Sub(mid).Abs(); best < 0 || d < bestDist {
				best, bestDist = j, d
			}
		}
		if best >= 0 {
			used[recs[best].ID] = true
			out = append(out, recs[best])
		}
	}
	slices.SortFunc(out, func(a, b sqlc.Recording) int { return a.StartedAt.Compare(b.StartedAt) })
	return out
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
