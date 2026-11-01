package liveauth

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"

	"github.com/google/uuid"
)

// TokenTTL is how long a live token can be used to start watching. Streams
// already started keep running (until the expireAt the auth endpoint
// returns), so this only bounds how long a leaked token is useful.
const TokenTTL = 5 * time.Minute

// maxClockSlack tolerates tokens whose expiry is slightly further away than
// TokenTTL (none are issued that way; it only absorbs clock adjustments).
const maxClockSlack = time.Minute

// Token layout (docs/design.md「ライブ用トークン」):
//
//	version (1) || session ID (16) || user ID (16) || expiry, Unix seconds, big endian (8) || HMAC-SHA256 (32)
//
// encoded as unpadded base64url. The MAC covers everything before it.
const (
	tokenVersion = 1
	payloadLen   = 1 + 16 + 16 + 8
	macLen       = sha256.Size
	rawTokenLen  = payloadLen + macLen
)

// hkdfInfo separates the token key from every other use of VMS_SECRET_KEY.
const hkdfInfo = "vms-demo live token v1"

var tokenEncoding = base64.RawURLEncoding.Strict()

// encodedTokenLen is the length of every token we issue.
var encodedTokenLen = tokenEncoding.EncodedLen(rawTokenLen)

// ErrInvalidToken is returned for tokens that are malformed, forged or
// expired. Callers must not tell these cases apart to clients.
var ErrInvalidToken = errors.New("liveauth: invalid or expired live token")

// Claims is what a valid token says.
type Claims struct {
	SessionID uuid.UUID
	UserID    uuid.UUID
	ExpiresAt time.Time
}

// Tokens issues and verifies live tokens. Tokens are not stored: they are
// MACed with a key derived from VMS_SECRET_KEY, so they stay valid across
// API restarts. Whether the session behind a token is still valid is
// checked separately (by the Decider) on every use.
type Tokens struct {
	key []byte
	now func() time.Time
}

// NewTokens derives the MAC key from secretKey (VMS_SECRET_KEY, 32 bytes).
// now may be nil (time.Now).
func NewTokens(secretKey []byte, now func() time.Time) (*Tokens, error) {
	if len(secretKey) < 32 {
		return nil, errors.New("liveauth: secret key must be at least 32 bytes")
	}
	key, err := hkdf.Key(sha256.New, secretKey, nil, hkdfInfo, sha256.Size)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Tokens{key: key, now: now}, nil
}

func (t *Tokens) mac(payload []byte) []byte {
	m := hmac.New(sha256.New, t.key)
	m.Write(payload)
	return m.Sum(nil)
}

// Issue returns a token for the session and user, valid for TokenTTL.
func (t *Tokens) Issue(sessionID, userID uuid.UUID) (string, time.Time) {
	exp := t.now().Add(TokenTTL).Truncate(time.Second)
	b := make([]byte, payloadLen, rawTokenLen)
	b[0] = tokenVersion
	copy(b[1:17], sessionID[:])
	copy(b[17:33], userID[:])
	binary.BigEndian.PutUint64(b[33:41], uint64(exp.Unix())) //nolint:gosec // G115: Unix time after 1970 is positive
	b = append(b, t.mac(b)...)
	return tokenEncoding.EncodeToString(b), exp
}

// Verify checks a token's format, MAC and expiry. Every failure is
// ErrInvalidToken.
func (t *Tokens) Verify(token string) (Claims, error) {
	if len(token) != encodedTokenLen {
		return Claims{}, ErrInvalidToken
	}
	b, err := tokenEncoding.DecodeString(token)
	if err != nil || len(b) != rawTokenLen {
		return Claims{}, ErrInvalidToken
	}
	payload, mac := b[:payloadLen], b[payloadLen:]
	// Check the MAC before looking at any field, in constant time.
	if !hmac.Equal(mac, t.mac(payload)) || payload[0] != tokenVersion {
		return Claims{}, ErrInvalidToken
	}
	secs := binary.BigEndian.Uint64(payload[33:41])
	if secs > uint64(1<<62) {
		return Claims{}, ErrInvalidToken
	}
	exp := time.Unix(int64(secs), 0)
	now := t.now()
	if !now.Before(exp) || exp.After(now.Add(TokenTTL+maxClockSlack)) {
		return Claims{}, ErrInvalidToken
	}
	var c Claims
	copy(c.SessionID[:], payload[1:17])
	copy(c.UserID[:], payload[17:33])
	c.ExpiresAt = exp
	return c, nil
}
