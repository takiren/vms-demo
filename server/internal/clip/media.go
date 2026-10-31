package clip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"github.com/takiren/vms-demo/server/internal/ffexec"
)

// Streams is what ffprobe reports about one segment.
type Streams struct {
	// StartTime is the first timestamp of the file in seconds (concat
	// inpoint/outpoint are in the file's own time).
	StartTime float64
	Video     []VideoStream
	Audio     []AudioStream
}

// VideoStream is the part of a video stream that must match across the
// segments of a clip (stream copy keeps the first segment's parameters).
type VideoStream struct {
	Codec   string
	Profile string
	Width   int
	Height  int
	PixFmt  string
}

// AudioStream is the part of an audio stream that must match across the
// segments of a clip.
type AudioStream struct {
	Codec      string
	SampleRate int
	Channels   int
}

// Media probes segments and concatenates them (FFmpeg in production).
type Media interface {
	// Probe reports the streams of a local file. It returns an error
	// wrapping ErrUnreadable when the file itself is the problem (ffprobe
	// ran and rejected it).
	Probe(ctx context.Context, file string) (Streams, error)
	// Concat joins the files of an ffconcat list into an mp4 by stream
	// copy.
	Concat(ctx context.Context, list, out string, opts ConcatOptions) error
}

// ConcatOptions configures Concat.
type ConcatOptions struct {
	// Audio copies the first audio stream; otherwise there is no audio.
	Audio bool
	// Duration, if positive, ends the output after this long (-t).
	Duration time.Duration
}

// ErrUnreadable means ffprobe could not read a segment (broken file).
var ErrUnreadable = errors.New("clip: segment is unreadable")

// FFmpeg implements Media with the LGPL ffmpeg/ffprobe CLI (ffexec).
type FFmpeg struct {
	FFmpegPath  string
	FFprobePath string
	// ProbeTimeout bounds one ffprobe run (ffexec.DefaultTimeout if 0).
	ProbeTimeout time.Duration
	// ConcatTimeout bounds one ffmpeg run (DefaultConcatTimeout if 0).
	ConcatTimeout time.Duration
}

// DefaultConcatTimeout bounds joining a clip. Stream copy of an hour of
// video takes seconds; this only catches a hung process.
const DefaultConcatTimeout = 10 * time.Minute

const (
	maxProbeOutput  = 64 << 10
	maxConcatOutput = 64 << 10
)

var _ Media = FFmpeg{}

type probeJSON struct {
	Streams []struct {
		CodecType  string `json:"codec_type"`
		CodecName  string `json:"codec_name"`
		Profile    string `json:"profile"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		PixFmt     string `json:"pix_fmt"`
		SampleRate string `json:"sample_rate"`
		Channels   int    `json:"channels"`
	} `json:"streams"`
	Format struct {
		StartTime string `json:"start_time"`
	} `json:"format"`
}

// Probe implements Media.
func (f FFmpeg) Probe(ctx context.Context, file string) (Streams, error) {
	out, err := ffexec.Run(ctx, f.FFprobePath, f.ProbeTimeout, maxProbeOutput,
		"-v", "error",
		"-show_entries", "stream=codec_type,codec_name,profile,width,height,pix_fmt,sample_rate,channels:format=start_time",
		"-of", "json",
		"file:"+file,
	)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return Streams{}, fmt.Errorf("%w: ffprobe: %w", ErrUnreadable, err)
		}
		return Streams{}, fmt.Errorf("ffprobe: %w", err)
	}
	return parseProbe(out)
}

func parseProbe(out []byte) (Streams, error) {
	var p probeJSON
	if err := json.Unmarshal(out, &p); err != nil {
		return Streams{}, fmt.Errorf("%w: ffprobe output: %w", ErrUnreadable, err)
	}
	var s Streams
	if p.Format.StartTime != "" {
		v, err := strconv.ParseFloat(p.Format.StartTime, 64)
		if err == nil && v >= 0 && v < 1e9 {
			s.StartTime = v
		}
	}
	for _, st := range p.Streams {
		switch st.CodecType {
		case "video":
			s.Video = append(s.Video, VideoStream{Codec: st.CodecName, Profile: st.Profile, Width: st.Width, Height: st.Height, PixFmt: st.PixFmt})
		case "audio":
			rate, _ := strconv.Atoi(st.SampleRate)
			s.Audio = append(s.Audio, AudioStream{Codec: st.CodecName, SampleRate: rate, Channels: st.Channels})
		}
	}
	return s, nil
}

// Concat implements Media. The list is read with -safe 1: it may only name
// plain relative files (ours: seg-NNNNN.mp4 next to it). Networking is not
// compiled in, and the list and output are given as file: URLs.
func (f FFmpeg) Concat(ctx context.Context, list, out string, opts ConcatOptions) error {
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "concat", "-safe", "1",
		"-i", "file:" + list,
		"-map", "0:v:0",
	}
	if opts.Audio {
		args = append(args, "-map", "0:a:0")
	} else {
		args = append(args, "-an")
	}
	if opts.Duration > 0 {
		args = append(args, "-t", strconv.FormatFloat(opts.Duration.Seconds(), 'f', 3, 64))
	}
	args = append(args,
		"-sn", "-dn",
		"-c", "copy",
		"-map_metadata", "-1",
		// The moov goes to the front (a second pass over the file) so players
		// can start before the download ends.
		"-movflags", "+faststart",
		"-f", "mp4", "-y",
		"file:"+out,
	)
	timeout := f.ConcatTimeout
	if timeout <= 0 {
		timeout = DefaultConcatTimeout
	}
	if _, err := ffexec.Run(ctx, f.FFmpegPath, timeout, maxConcatOutput, args...); err != nil {
		return fmt.Errorf("ffmpeg concat: %w", err)
	}
	return nil
}
