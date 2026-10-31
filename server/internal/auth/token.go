package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
)

// tokenBytes is the size of a session token before encoding.
const tokenBytes = 32

// tokenEncoding is base64url without padding, safe in a cookie value.
var tokenEncoding = base64.RawURLEncoding.Strict()

// encodedTokenLen is the length of an encoded session token.
var encodedTokenLen = tokenEncoding.EncodedLen(tokenBytes)

// newToken returns a fresh session token and its SHA-256, which is the only
// form stored in the database.
func newToken() (token string, hash []byte) {
	b := make([]byte, tokenBytes)
	// crypto/rand.Read never returns an error (it panics if the system
	// random source fails, which is the safe outcome).
	_, _ = rand.Read(b)
	sum := sha256.Sum256(b)
	return tokenEncoding.EncodeToString(b), sum[:]
}

// hashToken returns the SHA-256 of a session token taken from a cookie. It
// reports false for values that cannot be a token we issued, so malformed
// cookies never reach the database.
func hashToken(token string) ([]byte, bool) {
	if len(token) != encodedTokenLen {
		return nil, false
	}
	b, err := tokenEncoding.DecodeString(token)
	if err != nil || len(b) != tokenBytes {
		return nil, false
	}
	sum := sha256.Sum256(b)
	return sum[:], true
}

// CookieName is the session cookie. The __Host- prefix makes browsers
// require Secure and Path=/ and forbid Domain, so a sibling subdomain (such
// as s3.<host>) cannot set or overwrite it.
const CookieName = "__Host-vms_session"

// SessionCookie returns the Set-Cookie value that stores token. It has no
// Expires or Max-Age: the server enforces the session lifetime.
func SessionCookie(token string) string {
	return (&http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}).String()
}

// ClearSessionCookie returns the Set-Cookie value that deletes the session
// cookie.
func ClearSessionCookie() string {
	return (&http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1, // rendered as Max-Age=0
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}).String()
}
