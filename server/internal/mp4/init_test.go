package mp4_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/takiren/vms-demo/server/internal/mp4"
)

func box(typ string, payload int) []byte {
	b := make([]byte, 8+payload)
	binary.BigEndian.PutUint32(b, uint32(8+payload)) //nolint:gosec // test sizes are small
	copy(b[4:], typ)
	return b
}

func largeBox(typ string, payload int) []byte {
	b := make([]byte, 16+payload)
	binary.BigEndian.PutUint32(b, 1)
	copy(b[4:], typ)
	binary.BigEndian.PutUint64(b[8:], uint64(16+payload)) //nolint:gosec // test sizes are small
	return b
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func initSize(b []byte) (int64, error) {
	return mp4.InitSize(bytes.NewReader(b), int64(len(b)))
}

func TestInitSize(t *testing.T) {
	ftyp, moov := box("ftyp", 20), box("moov", 100)
	tests := []struct {
		name string
		data []byte
		want int64
		err  error
	}{
		{"ftyp moov moof mdat", cat(ftyp, moov, box("moof", 50), box("mdat", 500)), 136, nil},
		{"free between", cat(ftyp, moov, box("free", 4), box("moof", 50)), 148, nil},
		{"64-bit moov", cat(ftyp, largeBox("moov", 30), box("moof", 1)), 28 + 46, nil},
		{"64-bit mdat after moof", cat(ftyp, moov, box("moof", 1), largeBox("mdat", 3)), 136, nil},
		{"only moof header present", cat(ftyp, moov, box("moof", 0)[:8]), 136, nil},
		{"empty", nil, 0, mp4.ErrTruncated},
		{"short header", []byte{0, 0, 0}, 0, mp4.ErrTruncated},
		{"init only", cat(ftyp, moov), 0, mp4.ErrTruncated},
		{"cut in the next header", cat(ftyp, moov, []byte{0, 0, 0}), 0, mp4.ErrTruncated},
		{"cut in large size", cat(ftyp, largeBox("moov", 0)[:12]), 0, mp4.ErrTruncated},
		{"first box not ftyp", cat(moov, box("moof", 1)), 0, mp4.ErrInvalid},
		{"moof before moov", cat(ftyp, box("moof", 1), moov), 0, mp4.ErrInvalid},
		{"progressive mp4", cat(ftyp, box("mdat", 10), moov), 0, mp4.ErrInvalid},
		{"two moov", cat(ftyp, moov, moov, box("moof", 1)), 0, mp4.ErrInvalid},
		{"size zero", cat(ftyp, []byte{0, 0, 0, 0, 'm', 'o', 'o', 'v'}), 0, mp4.ErrInvalid},
		{"size smaller than header", cat(ftyp, []byte{0, 0, 0, 4, 'm', 'o', 'o', 'v'}), 0, mp4.ErrInvalid},
		{"large size smaller than header", cat(ftyp, []byte{0, 0, 0, 1, 'm', 'o', 'o', 'v', 0, 0, 0, 0, 0, 0, 0, 8}), 0, mp4.ErrInvalid},
		{"huge large size", cat(ftyp, []byte{0, 0, 0, 1, 'm', 'o', 'o', 'v', 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}), 0, mp4.ErrInvalid},
		{"garbage", bytes.Repeat([]byte{0xff}, 64), 0, mp4.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := initSize(tt.data)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("err = %v, want %v", err, tt.err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("InitSize = %d, %v; want %d", got, err, tt.want)
			}
		})
	}
}

func TestInitSizeBounds(t *testing.T) {
	// A huge box before any fragment is rejected without reading it.
	huge := box("moov", 0)
	binary.BigEndian.PutUint32(huge, mp4.MaxInitSize+100)
	if _, err := mp4.InitSize(bytes.NewReader(cat(box("ftyp", 4), huge)), 1<<40); !errors.Is(err, mp4.ErrInvalid) {
		t.Errorf("huge moov: %v", err)
	}
	// Many small boxes.
	parts := [][]byte{box("ftyp", 4), box("moov", 4)}
	for range 100 {
		parts = append(parts, box("free", 0))
	}
	parts = append(parts, box("moof", 0))
	if _, err := initSize(cat(parts...)); !errors.Is(err, mp4.ErrInvalid) {
		t.Errorf("many boxes: %v", err)
	}
}

// The real test segments (ffmpeg-made fMP4, like MediaMTX's layout).
func TestInitSizeTestdata(t *testing.T) {
	files, _ := filepath.Glob("../../../docker/ffmpeg/testdata/*.mp4")
	if len(files) == 0 {
		t.Fatal("no testdata")
	}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // G304: test data
		if err != nil {
			t.Fatal(err)
		}
		n, err := initSize(b)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if string(b[n+4:n+8]) != "moof" || string(b[4:8]) != "ftyp" {
			t.Errorf("%s: init size %d does not end at a moof", f, n)
		}
		// A prefix that cuts the moov is truncated, not invalid.
		if _, err := initSize(b[:n-1]); !errors.Is(err, mp4.ErrTruncated) {
			t.Errorf("%s prefix: %v", f, err)
		}
	}
}

func FuzzInitSize(f *testing.F) {
	f.Add(cat(box("ftyp", 4), box("moov", 4), box("moof", 0)))
	f.Add([]byte{0, 0, 0, 1, 'f', 't', 'y', 'p'})
	f.Fuzz(func(t *testing.T, b []byte) {
		n, err := initSize(b)
		if err == nil && (n < 16 || n > int64(len(b))-8 || string(b[n+4:n+8]) != "moof") {
			t.Fatalf("InitSize = %d for %x", n, b)
		}
	})
}
