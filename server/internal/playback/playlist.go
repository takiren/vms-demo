package playback

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Segment is one recording in an HLS playlist. The same URL serves the
// init section (bytes [0, InitSize)) and the fragments ([InitSize, Size)).
type Segment struct {
	URL      string
	Start    time.Time
	Duration time.Duration
	InitSize int64
	Size     int64
}

// pdtLayout is EXT-X-PROGRAM-DATE-TIME's format (ISO 8601 with milliseconds).
const pdtLayout = "2006-01-02T15:04:05.000Z07:00"

// errBadSegment means a segment cannot be written into a playlist.
var errBadSegment = errors.New("playback: invalid segment")

// seconds formats d in seconds with millisecond precision.
func seconds(d time.Duration) string {
	return fmt.Sprintf("%.3f", d.Round(time.Millisecond).Seconds())
}

// BuildPlaylist writes an HLS VOD media playlist (version 7, fMP4 with byte
// ranges) of segs, which must be in time order. Every segment after the
// first starts a discontinuity: each file has its own init section and
// timestamps, and gaps between recordings are collapsed. startOffset, when
// positive, makes players start that far into the playlist (EXT-X-START).
func BuildPlaylist(segs []Segment, startOffset time.Duration) ([]byte, error) {
	if len(segs) == 0 {
		return nil, fmt.Errorf("%w: no segments", errBadSegment)
	}
	var maxDur time.Duration
	for i, s := range segs {
		switch {
		case s.Duration < time.Millisecond:
			return nil, fmt.Errorf("%w: segment %d has no duration", errBadSegment, i)
		case s.InitSize <= 0 || s.Size <= s.InitSize:
			return nil, fmt.Errorf("%w: segment %d has byte range %d/%d", errBadSegment, i, s.InitSize, s.Size)
		case s.URL == "" || strings.ContainsAny(s.URL, "\"\r\n"):
			// The URL goes into a quoted attribute and a line of its own.
			return nil, fmt.Errorf("%w: segment %d has an unusable URL", errBadSegment, i)
		}
		maxDur = max(maxDur, s.Duration.Round(time.Millisecond))
	}
	// EXTINF rounded to the nearest integer must not exceed the target.
	target := max(1, int(math.Ceil(maxDur.Seconds())))

	var b bytes.Buffer
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", target)
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	// MediaMTX starts every segment at a keyframe.
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	if startOffset > 0 {
		fmt.Fprintf(&b, "#EXT-X-START:TIME-OFFSET=%s,PRECISE=YES\n", seconds(startOffset))
	}
	for i, s := range segs {
		if i > 0 {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\",BYTERANGE=\"%d@0\"\n", s.URL, s.InitSize)
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", s.Start.UTC().Format(pdtLayout))
		fmt.Fprintf(&b, "#EXTINF:%s,\n", seconds(s.Duration))
		fmt.Fprintf(&b, "#EXT-X-BYTERANGE:%d@%d\n", s.Size-s.InitSize, s.InitSize)
		b.WriteString(s.URL)
		b.WriteByte('\n')
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.Bytes(), nil
}
