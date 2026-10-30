package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Password policy (docs/design.md「パスワード」). Lengths are counted in
// Unicode code points, the same unit as JSON Schema minLength/maxLength in
// api/openapi.yaml. There are no composition rules and no normalization.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 128
)

// ErrWeakPassword is returned for passwords that do not meet the policy.
var ErrWeakPassword = fmt.Errorf("password must be %d to %d characters", MinPasswordLength, MaxPasswordLength)

// placeholderPassword is the value .env.example ships with.
const placeholderPassword = "CHANGE_ME"

// ValidatePassword checks a new password against the policy. The error never
// contains the password.
func ValidatePassword(pw string) error {
	if !utf8.ValidString(pw) {
		return ErrWeakPassword
	}
	n := utf8.RuneCountInString(pw)
	if n < MinPasswordLength || n > MaxPasswordLength {
		return ErrWeakPassword
	}
	if strings.Contains(strings.ToUpper(pw), placeholderPassword) {
		return errors.New("password must not contain the " + placeholderPassword + " placeholder")
	}
	return nil
}

// Params are argon2id parameters. They are stored in every hash (PHC string
// format), so they can be changed later; hashes with other parameters are
// rehashed on the next successful login.
type Params struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
	SaltLen   uint32
	KeyLen    uint32
}

// DefaultParams is the second recommended option of RFC 9106 (section 4):
// 64 MiB of memory, 3 passes, 4 lanes, 128-bit salt, 256-bit tag.
var DefaultParams = Params{MemoryKiB: 64 * 1024, Time: 3, Threads: 4, SaltLen: 16, KeyLen: 32}

// Bounds accepted when parsing stored hashes. They keep a corrupt or hostile
// hash from making verification take unbounded memory or time.
const (
	maxMemoryKiB = 1 << 20 // 1 GiB
	maxTime      = 16
	minSaltLen   = 8
	maxSaltLen   = 64
	minKeyLen    = 16
	maxKeyLen    = 64
)

// ErrBusy means too many password hashes are being computed and the wait
// for a free slot timed out.
var ErrBusy = errors.New("auth: too many concurrent password hash computations")

// errMalformedHash is returned for stored hashes that cannot be parsed.
var errMalformedHash = errors.New("auth: malformed password hash")

// Hasher hashes and verifies passwords with argon2id. It limits how many
// hashes are computed at once, because each one takes Params.MemoryKiB of
// memory; a flood of login requests must not exhaust memory.
type Hasher struct {
	params  Params
	sem     chan struct{}
	maxWait time.Duration
}

// NewHasher returns a Hasher that computes at most maxConcurrent hashes at a
// time and waits at most maxWait for a free slot before failing with ErrBusy.
func NewHasher(p Params, maxConcurrent int, maxWait time.Duration) *Hasher {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Hasher{params: p, sem: make(chan struct{}, maxConcurrent), maxWait: maxWait}
}

func (h *Hasher) acquire(ctx context.Context) error {
	t := time.NewTimer(h.maxWait)
	defer t.Stop()
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-t.C:
		return ErrBusy
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrBusy, ctx.Err())
	}
}

func (h *Hasher) release() { <-h.sem }

// Hash returns the PHC string of password with a fresh random salt.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: salt: %w", err)
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	key := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.MemoryKiB, h.params.Threads, h.params.KeyLen)
	return encodePHC(h.params, salt, key), nil
}

// Verify reports whether password matches the PHC string encoded. When it
// matches, needsRehash reports whether encoded uses parameters other than
// the Hasher's, so the caller should store a new hash.
func (h *Hasher) Verify(ctx context.Context, encoded, password string) (ok, needsRehash bool, err error) {
	p, salt, want, err := decodePHC(encoded)
	if err != nil {
		return false, false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	defer h.release()
	got := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, p.KeyLen)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	return true, p != h.params, nil
}

var b64 = base64.RawStdEncoding.Strict()

func encodePHC(p Params, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// decodePHC parses $argon2id$v=19$m=<kib>,t=<passes>,p=<lanes>$<salt>$<hash>.
func decodePHC(s string) (Params, []byte, []byte, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, errMalformedHash
	}
	if parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return Params{}, nil, nil, fmt.Errorf("%w: unsupported version", errMalformedHash)
	}

	var p Params
	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return Params{}, nil, nil, errMalformedHash
	}
	for i, f := range fields {
		name, val, ok := strings.Cut(f, "=")
		if !ok || name != [...]string{"m", "t", "p"}[i] {
			return Params{}, nil, nil, errMalformedHash
		}
		n, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			return Params{}, nil, nil, errMalformedHash
		}
		switch name {
		case "m":
			p.MemoryKiB = uint32(n)
		case "t":
			p.Time = uint32(n)
		case "p":
			if n > 255 {
				return Params{}, nil, nil, errMalformedHash
			}
			p.Threads = uint8(n)
		}
	}
	if p.Threads < 1 || p.Time < 1 || p.Time > maxTime ||
		p.MemoryKiB < 8*uint32(p.Threads) || p.MemoryKiB > maxMemoryKiB {
		return Params{}, nil, nil, fmt.Errorf("%w: parameters out of range", errMalformedHash)
	}

	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < minSaltLen || len(salt) > maxSaltLen {
		return Params{}, nil, nil, fmt.Errorf("%w: salt", errMalformedHash)
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) < minKeyLen || len(key) > maxKeyLen {
		return Params{}, nil, nil, fmt.Errorf("%w: hash", errMalformedHash)
	}
	p.SaltLen = uint32(len(salt)) //nolint:gosec // G115: bounded by maxSaltLen above
	p.KeyLen = uint32(len(key))   //nolint:gosec // G115: bounded by maxKeyLen above
	return p, salt, key, nil
}
