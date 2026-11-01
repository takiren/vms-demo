package mediamtx

import (
	"strings"

	"github.com/google/uuid"
)

// ParsePath parses a path name made by MainPath or SubPath. It accepts only
// the exact form those produce (lower-case canonical UUID), so that one
// camera has exactly two possible path names. sub reports the _sub path.
func ParsePath(name string) (id uuid.UUID, sub bool, ok bool) {
	rest, found := strings.CutPrefix(name, ManagedPrefix)
	if !found {
		return uuid.UUID{}, false, false
	}
	var raw string
	switch {
	case strings.HasSuffix(rest, "_main"):
		raw = strings.TrimSuffix(rest, "_main")
	case strings.HasSuffix(rest, "_sub"):
		raw, sub = strings.TrimSuffix(rest, "_sub"), true
	default:
		return uuid.UUID{}, false, false
	}
	id, err := uuid.Parse(raw)
	if err != nil || id.String() != raw {
		return uuid.UUID{}, false, false
	}
	return id, sub, true
}
