package playback

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)

func TestBuildPlaylistGolden(t *testing.T) {
	segs := []Segment{
		{URL: "https://s3.vms.example/recordings/a.mp4?X-Amz-Signature=1", Start: t0, Duration: 60*time.Second + 2*time.Millisecond, InitSize: 1219, Size: 9000},
		// Contiguous with the first one.
		{URL: "https://s3.vms.example/recordings/b.mp4?X-Amz-Signature=2", Start: t0.Add(60 * time.Second), Duration: 59500 * time.Millisecond, InitSize: 1300, Size: 8000},
		// After a 10-minute gap, in another zone (written as UTC).
		{URL: "https://s3.vms.example/recordings/c.mp4?X-Amz-Signature=3", Start: t0.Add(11 * time.Minute).In(time.FixedZone("JST", 9*3600)), Duration: 1500 * time.Millisecond, InitSize: 1000, Size: 1001},
	}
	got, err := BuildPlaylist(segs, 30*time.Second+250*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	want := `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:61
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-START:TIME-OFFSET=30.250,PRECISE=YES
#EXT-X-MAP:URI="https://s3.vms.example/recordings/a.mp4?X-Amz-Signature=1",BYTERANGE="1219@0"
#EXT-X-PROGRAM-DATE-TIME:2026-01-02T03:00:00.000Z
#EXTINF:60.002,
#EXT-X-BYTERANGE:7781@1219
https://s3.vms.example/recordings/a.mp4?X-Amz-Signature=1
#EXT-X-DISCONTINUITY
#EXT-X-MAP:URI="https://s3.vms.example/recordings/b.mp4?X-Amz-Signature=2",BYTERANGE="1300@0"
#EXT-X-PROGRAM-DATE-TIME:2026-01-02T03:01:00.000Z
#EXTINF:59.500,
#EXT-X-BYTERANGE:6700@1300
https://s3.vms.example/recordings/b.mp4?X-Amz-Signature=2
#EXT-X-DISCONTINUITY
#EXT-X-MAP:URI="https://s3.vms.example/recordings/c.mp4?X-Amz-Signature=3",BYTERANGE="1000@0"
#EXT-X-PROGRAM-DATE-TIME:2026-01-02T03:11:00.000Z
#EXTINF:1.500,
#EXT-X-BYTERANGE:1@1000
https://s3.vms.example/recordings/c.mp4?X-Amz-Signature=3
#EXT-X-ENDLIST
`
	if string(got) != want {
		t.Errorf("playlist:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildPlaylistTargetDuration(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{60 * time.Second, "#EXT-X-TARGETDURATION:60\n"},
		{60*time.Second + 400*time.Microsecond, "#EXT-X-TARGETDURATION:60\n"}, // rounds to 60.000
		{60*time.Second + time.Millisecond, "#EXT-X-TARGETDURATION:61\n"},
		{200 * time.Millisecond, "#EXT-X-TARGETDURATION:1\n"},
	} {
		got, err := BuildPlaylist([]Segment{{URL: "u", Start: t0, Duration: tc.d, InitSize: 1, Size: 2}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), tc.want) {
			t.Errorf("%s: %s", tc.d, got)
		}
		if strings.Contains(string(got), "EXT-X-START") || strings.Contains(string(got), "DISCONTINUITY") {
			t.Errorf("single segment without offset: %s", got)
		}
	}
}

func TestBuildPlaylistRejects(t *testing.T) {
	ok := Segment{URL: "u", Start: t0, Duration: time.Second, InitSize: 1, Size: 2}
	for name, mod := range map[string]func(*Segment){
		"no duration":     func(s *Segment) { s.Duration = 0 },
		"no init":         func(s *Segment) { s.InitSize = 0 },
		"no media":        func(s *Segment) { s.Size = s.InitSize },
		"empty url":       func(s *Segment) { s.URL = "" },
		"quote in url":    func(s *Segment) { s.URL = `u"x` },
		"newline in url":  func(s *Segment) { s.URL = "u\n#EXT-X-ENDLIST" },
		"carriage return": func(s *Segment) { s.URL = "u\r" },
	} {
		s := ok
		mod(&s)
		if _, err := BuildPlaylist([]Segment{ok, s}, 0); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := BuildPlaylist(nil, 0); err == nil {
		t.Error("empty playlist accepted")
	}
}

func box(typ string, payload int) []byte {
	b := make([]byte, 8+payload)
	n := 8 + payload
	b[0], b[1], b[2], b[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	copy(b[4:], typ)
	return b
}
