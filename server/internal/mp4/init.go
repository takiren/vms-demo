// Package mp4 reads the top-level box structure of fragmented MP4 files
// (ISO/IEC 14496-12) as MediaMTX records them: an initialization section
// (ftyp, moov) followed by fragments (moof, mdat). Only box headers are
// read; box contents are never parsed.
package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxInitSize bounds the initialization section. MediaMTX writes a few KB;
// anything near this is not a recording we made.
const MaxInitSize = 4 << 20

// maxBoxes bounds the number of top-level boxes before the first moof.
const maxBoxes = 64

var (
	// ErrTruncated means the data ended before the first moof box. With a
	// prefix of a file, read more; with a whole file, it is not fragmented.
	ErrTruncated = errors.New("mp4: data ends before the first fragment")
	// ErrInvalid means the data is not a fragmented MP4 we can split.
	ErrInvalid = errors.New("mp4: not a fragmented MP4 with an init section")
)

// InitSize returns the length of the initialization section of a fragmented
// MP4: the offset of the first top-level moof box. The file must start with
// ftyp and have a moov before the first moof. size is the number of bytes
// readable from r (a whole file or a prefix of one).
//
// It returns ErrTruncated when r ends before a moof box header and
// ErrInvalid (wrapped) for anything malformed. It never reads beyond
// MaxInitSize.
func InitSize(r io.ReaderAt, size int64) (int64, error) {
	var (
		off      int64
		seenMoov bool
		hdr      [16]byte
	)
	for i := 0; ; i++ {
		if i >= maxBoxes {
			return 0, fmt.Errorf("%w: more than %d boxes before the first fragment", ErrInvalid, maxBoxes)
		}
		if off > MaxInitSize {
			return 0, fmt.Errorf("%w: init section is larger than %d bytes", ErrInvalid, MaxInitSize)
		}
		if size-off < 8 {
			return 0, ErrTruncated
		}
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return 0, readErr(err)
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		headerLen := int64(8)
		switch boxSize {
		case 0:
			// The box extends to the end of the file. Only the last box may do
			// that, and there would be no fragment after it.
			return 0, fmt.Errorf("%w: %q box extends to the end of the file", ErrInvalid, printable(typ))
		case 1:
			if size-off < 16 {
				return 0, ErrTruncated
			}
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return 0, readErr(err)
			}
			large := binary.BigEndian.Uint64(hdr[8:16])
			// Bounded so off+boxSize cannot overflow (off <= MaxInitSize).
			if large > 1<<62 {
				return 0, fmt.Errorf("%w: %q box size overflows", ErrInvalid, printable(typ))
			}
			boxSize = int64(large) //nolint:gosec // G115: bounded above
			headerLen = 16
		}
		if boxSize < headerLen {
			return 0, fmt.Errorf("%w: %q box size %d is smaller than its header", ErrInvalid, printable(typ), boxSize)
		}

		switch {
		case i == 0 && typ != "ftyp":
			return 0, fmt.Errorf("%w: first box is %q, not ftyp", ErrInvalid, printable(typ))
		case typ == "moof":
			if !seenMoov {
				return 0, fmt.Errorf("%w: moof before moov", ErrInvalid)
			}
			return off, nil
		case typ == "mdat":
			// Media data before any fragment: a progressive MP4.
			return 0, fmt.Errorf("%w: mdat before the first moof", ErrInvalid)
		case typ == "moov":
			if seenMoov {
				return 0, fmt.Errorf("%w: more than one moov", ErrInvalid)
			}
			seenMoov = true
		}
		off += boxSize
	}
}

func readErr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrTruncated
	}
	return fmt.Errorf("mp4: read: %w", err)
}

// printable keeps box types from garbage input safe to log.
func printable(typ string) string {
	b := []byte(typ)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}
