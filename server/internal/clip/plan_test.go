package clip

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFilenames(t *testing.T) {
	from := time.Date(2026, 1, 2, 12, 0, 0, 0, time.FixedZone("JST", 9*3600))
	to := from.Add(30 * time.Minute)
	const ts = "_20260102T030000Z-20260102T033000Z.mp4"
	for _, tc := range []struct{ camera, ascii, utf8 string }{
		{"Front Door", "Front_Door", "Front_Door"},
		{"玄関カメラ 1", "1", "玄関カメラ_1"},
		{"玄関", "camera", "玄関"},
		{"../../etc/passwd", "etc_passwd", "etc_passwd"},
		{`a"b\c;d`, "a_b_c_d", "a_b_c_d"},
		{"..", "camera", "camera"},
		{"", "camera", "camera"},
		{"cam\r\nSet-Cookie: x=1", "cam_Set-Cookie_x_1", "cam_Set-Cookie_x_1"},
		{"bad\xffutf8", "bad_utf8", "bad_utf8"},
		{"Lobby (2F) [east]", "Lobby_2F_east", "Lobby_(2F)_[east]"},
	} {
		a, u := Filenames(tc.camera, from, to)
		if a != tc.ascii+ts || u != tc.utf8+ts {
			t.Errorf("Filenames(%q) = %q, %q; want %q, %q", tc.camera, a, u, tc.ascii+ts, tc.utf8+ts)
		}
	}
	long := strings.Repeat("あ", 200)
	_, u := Filenames(long, from, to)
	if n := len([]rune(strings.TrimSuffix(u, ts))); n != maxNameRunes {
		t.Errorf("long name kept %d runes", n)
	}
}

func TestContentDisposition(t *testing.T) {
	got := ContentDisposition("1_x.mp4", "玄関 1_x.mp4")
	want := `attachment; filename="1_x.mp4"; filename*=UTF-8''%E7%8E%84%E9%96%A2%201_x.mp4`
	if got != want {
		t.Errorf("ContentDisposition = %s", got)
	}
	for _, c := range []string{"\r", "\n", `"`, ";", "'", " "} {
		if strings.Contains(encodeRFC5987("a"+c+"b"), c) {
			t.Errorf("%q not encoded", c)
		}
	}
}

func TestConcatList(t *testing.T) {
	entries := []listEntry{
		{Name: segmentFile(0), Inpoint: 12.5},
		{Name: segmentFile(1), Inpoint: -1},
	}
	got, err := concatList(entries)
	want := "ffconcat version 1.0\n" +
		"file 'seg-00000.mp4'\ninpoint 12.500000\n" +
		"file 'seg-00001.mp4'\n"
	if err != nil || got != want {
		t.Fatalf("concatList = %q, %v", got, err)
	}
	if _, err := concatList(nil); err == nil {
		t.Error("empty list accepted")
	}
	for _, bad := range []string{"x'.mp4", "../seg-00000.mp4", "/tmp/seg-00000.mp4", "file:seg-00000.mp4", "seg-1.mp4", "seg-00000.mp4\nfile '/etc/passwd'"} {
		if _, err := concatList([]listEntry{{Name: bad, Inpoint: -1}}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestTrimPlan(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	// Two one-minute recordings with a 30-second gap between them.
	segs := []segment{
		{Start: t0, End: t0.Add(time.Minute), StartTime: 0.5},
		{Start: t0.Add(90 * time.Second), End: t0.Add(150 * time.Second)},
	}
	for _, tc := range []struct {
		name     string
		from, to time.Time
		in       float64
		limit    time.Duration
	}{
		{"covers everything", t0.Add(-time.Hour), t0.Add(time.Hour), -1, 0},
		{"cut both ends", t0.Add(10 * time.Second), t0.Add(120 * time.Second), 10.5, 50*time.Second + 30*time.Second},
		{"cut the start", t0.Add(time.Second), t0.Add(150 * time.Second), 1.5, 0},
		{"cut the end", t0, t0.Add(100 * time.Second), -1, time.Minute + 10*time.Second},
	} {
		in, limit := trimPlan(segs, tc.from, tc.to)
		if in != tc.in || limit != tc.limit {
			t.Errorf("%s: inpoint %v limit %v; want %v %v", tc.name, in, limit, tc.in, tc.limit)
		}
	}
	if in, limit := trimPlan(nil, t0, t0); in != -1 || limit != 0 {
		t.Error("empty plan")
	}
}

func TestCheckVideo(t *testing.T) {
	h264 := VideoStream{Codec: "h264", Profile: "High", Width: 1920, Height: 1080, PixFmt: "yuv420p"}
	starts := []time.Time{time.Unix(0, 0), time.Unix(60, 0), time.Unix(120, 0)}
	ok := Streams{Video: []VideoStream{h264}}
	if err := checkVideo([]Streams{ok, ok, ok}, starts); err != nil {
		t.Error(err)
	}
	if err := checkVideo(nil, nil); err != nil {
		t.Error(err)
	}
	changed := h264
	changed.Width = 1280
	for name, probes := range map[string][]Streams{
		"resolution changes": {ok, ok, {Video: []VideoStream{changed}}},
		"no video":           {ok, {}},
		"codec":              {{Video: []VideoStream{{Codec: "mjpeg"}}}},
	} {
		err := checkVideo(probes, starts)
		if !errors.Is(err, ErrIncompatible) {
			t.Errorf("%s: %v", name, err)
		}
	}
	err := checkVideo([]Streams{ok, ok, {Video: []VideoStream{changed}}}, starts)
	if !strings.Contains(err.Error(), "1970-01-01T00:02:00Z") {
		t.Errorf("message does not say where: %v", err)
	}
}

func TestDecideAudio(t *testing.T) {
	aac := AudioStream{Codec: "aac", SampleRate: 48000, Channels: 1}
	v := []VideoStream{{Codec: "h264"}}
	with := func(a ...AudioStream) Streams { return Streams{Video: v, Audio: a} }
	for _, tc := range []struct {
		name   string
		probes []Streams
		want   AudioMode
	}{
		{"aac everywhere", []Streams{with(aac), with(aac)}, AudioIncluded},
		{"opus", []Streams{with(AudioStream{Codec: "opus", SampleRate: 48000, Channels: 2})}, AudioIncluded},
		{"no audio", []Streams{with(), with()}, AudioNone},
		{"G.711 a-law", []Streams{with(AudioStream{Codec: "pcm_alaw", SampleRate: 8000, Channels: 1})}, AudioDropped},
		{"G.711 mu-law", []Streams{with(AudioStream{Codec: "pcm_mulaw", SampleRate: 8000, Channels: 1})}, AudioDropped},
		{"some segments without audio", []Streams{with(aac), with()}, AudioDropped},
		{"sample rate changes", []Streams{with(aac), with(AudioStream{Codec: "aac", SampleRate: 16000, Channels: 1})}, AudioDropped},
	} {
		if got := decideAudio(tc.probes); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestParseProbe(t *testing.T) {
	out := `{"programs":[],"stream_groups":[],"streams":[
	 {"index":0,"codec_name":"h264","profile":"High","codec_type":"video","width":160,"height":90,"pix_fmt":"yuv420p"},
	 {"index":1,"codec_name":"aac","profile":"LC","codec_type":"audio","sample_rate":"44100","channels":1},
	 {"index":2,"codec_type":"data"}],
	 "format":{"start_time":"0.200000","duration":"1.200000"}}`
	s, err := parseProbe([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := Streams{
		StartTime: 0.2,
		Video:     []VideoStream{{Codec: "h264", Profile: "High", Width: 160, Height: 90, PixFmt: "yuv420p"}},
		Audio:     []AudioStream{{Codec: "aac", SampleRate: 44100, Channels: 1}},
	}
	if s.StartTime != want.StartTime || len(s.Video) != 1 || s.Video[0] != want.Video[0] || len(s.Audio) != 1 || s.Audio[0] != want.Audio[0] {
		t.Errorf("parseProbe = %+v", s)
	}
	if _, err := parseProbe([]byte("not json")); !errors.Is(err, ErrUnreadable) {
		t.Errorf("bad output: %v", err)
	}
	s, _ = parseProbe([]byte(`{"streams":[],"format":{"start_time":"N/A"}}`))
	if s.StartTime != 0 {
		t.Errorf("N/A start time = %v", s.StartTime)
	}
}
