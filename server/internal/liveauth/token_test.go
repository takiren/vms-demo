package liveauth

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var (
	testKey   = bytes.Repeat([]byte{0x42}, 32)
	testSID   = uuid.MustParse("01900000-0000-7000-8000-0000000000aa")
	testUID   = uuid.MustParse("01900000-0000-7000-8000-000000000002")
	testEpoch = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestTokens(t *testing.T, c *clock) *Tokens {
	t.Helper()
	tk, err := NewTokens(testKey, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestTokenRoundTrip(t *testing.T) {
	c := &clock{testEpoch}
	tk := newTestTokens(t, c)
	tok, exp := tk.Issue(testSID, testUID)
	if !exp.Equal(testEpoch.Add(TokenTTL)) {
		t.Errorf("expiry = %v", exp)
	}
	if len(tok) != encodedTokenLen || strings.ContainsAny(tok, ":=+/") {
		t.Errorf("token %q is not unpadded base64url of the expected length", tok)
	}
	got, err := tk.Verify(tok)
	if err != nil || got.SessionID != testSID || got.UserID != testUID || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("Verify = %+v, %v", got, err)
	}

	// Valid until just before the expiry, not at it.
	c.t = exp.Add(-time.Second)
	if _, err := tk.Verify(tok); err != nil {
		t.Errorf("just before expiry: %v", err)
	}
	c.t = exp
	if _, err := tk.Verify(tok); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("at expiry: %v", err)
	}
}

func TestTokenKeyIsBoundToTheSecret(t *testing.T) {
	c := &clock{testEpoch}
	tok, _ := newTestTokens(t, c).Issue(testSID, testUID)

	// Same secret (another process, after a restart): still valid.
	if _, err := newTestTokens(t, c).Verify(tok); err != nil {
		t.Errorf("same key: %v", err)
	}
	other, err := NewTokens(bytes.Repeat([]byte{0x43}, 32), c.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Verify(tok); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("other key: %v", err)
	}
	// The MAC key is derived, not VMS_SECRET_KEY itself.
	if bytes.Equal(newTestTokens(t, c).key, testKey) {
		t.Error("the secret key is used directly")
	}
	if _, err := NewTokens(make([]byte, 16), nil); err == nil {
		t.Error("short key accepted")
	}
}

func TestTokenRejectsTampering(t *testing.T) {
	c := &clock{testEpoch}
	tk := newTestTokens(t, c)
	tok, _ := tk.Issue(testSID, testUID)
	raw, err := tokenEncoding.DecodeString(tok)
	if err != nil {
		t.Fatal(err)
	}

	// Flipping any bit (payload or MAC) invalidates the token.
	for i := range raw {
		b := bytes.Clone(raw)
		b[i] ^= 0x01
		if _, err := tk.Verify(tokenEncoding.EncodeToString(b)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("bit flip at byte %d accepted", i)
		}
	}

	// A correctly MACed token with another version, or an expiry too far in
	// the future, is still rejected.
	reMAC := func(mutate func([]byte)) string {
		b := bytes.Clone(raw[:payloadLen])
		mutate(b)
		return tokenEncoding.EncodeToString(append(b, tk.mac(b)...))
	}
	if _, err := tk.Verify(reMAC(func(b []byte) { b[0] = 2 })); !errors.Is(err, ErrInvalidToken) {
		t.Error("version 2 accepted")
	}
	if _, err := tk.Verify(reMAC(func(b []byte) {
		binary.BigEndian.PutUint64(b[33:41], uint64(testEpoch.Add(24*time.Hour).Unix()))
	})); !errors.Is(err, ErrInvalidToken) {
		t.Error("long-lived token accepted")
	}
	if _, err := tk.Verify(reMAC(func(b []byte) { binary.BigEndian.PutUint64(b[33:41], 1<<63) })); !errors.Is(err, ErrInvalidToken) {
		t.Error("overflowing expiry accepted")
	}
	if _, err := tk.Verify(reMAC(func([]byte) {})); err != nil {
		t.Errorf("re-MACed original: %v", err)
	}

	for _, bad := range []string{
		"",
		tok[:len(tok)-1],
		tok + "A",
		strings.Repeat("A", encodedTokenLen),
		"=" + tok[1:],
		strings.Replace(tok, tok[:1], "+", 1),
		"vms-api:" + tok[8:],
	} {
		if _, err := tk.Verify(bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Verify(%q) = %v", bad, err)
		}
	}
}
