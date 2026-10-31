package clip

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// maxNameRunes bounds the camera part of a download's file name.
const maxNameRunes = 64

// fileTimeLayout is the timestamp format in file names (UTC, no colons:
// Windows does not allow them in file names).
const fileTimeLayout = "20060102T150405Z"

// Filenames returns the download's file name as plain ASCII (for the
// filename parameter, understood by every client) and as UTF-8 (for
// filename*, RFC 6266/5987). Both are <camera>_<from>-<to>.mp4 with the
// times in UTC.
func Filenames(camera string, from, to time.Time) (ascii, utf8Name string) {
	suffix := "_" + from.UTC().Format(fileTimeLayout) + "-" + to.UTC().Format(fileTimeLayout) + ".mp4"
	return asciiName(camera) + suffix, unicodeName(camera) + suffix
}

// asciiName keeps A-Z a-z 0-9 . _ - and turns runs of anything else into
// one underscore. Leading and trailing separators are dropped (no hidden
// files, no "..").
func asciiName(s string) string {
	var b strings.Builder
	underscore := false
	n := 0
	for _, r := range s {
		if n >= maxNameRunes {
			break
		}
		ok := r < utf8.RuneSelf && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_')
		switch {
		case ok:
			b.WriteRune(r)
			underscore = false
			n++
		case !underscore:
			b.WriteByte('_')
			underscore = true
			n++
		}
	}
	return trimName(b.String())
}

// unicodeName keeps letters, digits and marks of any script and a few
// punctuation characters; everything else (controls, path separators,
// characters Windows rejects, quotes) becomes an underscore.
func unicodeName(s string) string {
	var b strings.Builder
	underscore := false
	n := 0
	for _, r := range s {
		if n >= maxNameRunes {
			break
		}
		ok := r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) ||
			r == '.' || r == '-' || r == '_' || r == '(' || r == ')' || r == '[' || r == ']')
		switch {
		case ok:
			b.WriteRune(r)
			underscore = false
			n++
		case !underscore:
			b.WriteByte('_')
			underscore = true
			n++
		}
	}
	return trimName(b.String())
}

func trimName(s string) string {
	s = strings.Trim(s, "._-")
	if s == "" {
		return "camera"
	}
	return s
}

// ContentDisposition returns an attachment Content-Disposition with both
// filename (ASCII, needs no quoting: asciiName allows no quote or
// backslash) and filename* (UTF-8, percent-encoded).
func ContentDisposition(ascii, utf8Name string) string {
	return `attachment; filename="` + ascii + `"; filename*=UTF-8''` + encodeRFC5987(utf8Name)
}

// encodeRFC5987 percent-encodes every byte outside attr-char (RFC 5987).
func encodeRFC5987(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$&+-.^_`|~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0xf])
	}
	return b.String()
}
