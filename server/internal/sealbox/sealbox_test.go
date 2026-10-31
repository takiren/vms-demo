package sealbox_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/takiren/vms-demo/server/internal/sealbox"
)

func newBox(t *testing.T, fill byte) *sealbox.Box {
	t.Helper()
	b, err := sealbox.New(bytes.Repeat([]byte{fill}, sealbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	b := newBox(t, 1)
	for _, pt := range [][]byte{nil, []byte("x"), bytes.Repeat([]byte("secret"), 1000)} {
		blob, err := b.Seal(pt, []byte("camera:1"))
		if err != nil {
			t.Fatal(err)
		}
		if blob[0] != sealbox.Version1 {
			t.Errorf("version byte = %d", blob[0])
		}
		// Short plaintexts can appear in random bytes by chance; only check
		// ones long enough for a match to mean the plaintext leaked.
		if len(pt) >= 16 && bytes.Contains(blob, pt) {
			t.Error("plaintext visible in blob")
		}
		got, err := b.Open(blob, []byte("camera:1"))
		if err != nil || !bytes.Equal(got, pt) {
			t.Errorf("Open = %q, %v", got, err)
		}
	}
}

func TestNonceIsRandom(t *testing.T) {
	b := newBox(t, 1)
	a1, _ := b.Seal([]byte("same"), nil)
	a2, _ := b.Seal([]byte("same"), nil)
	if bytes.Equal(a1, a2) {
		t.Error("two seals of the same plaintext are identical")
	}
	if bytes.Equal(a1[1:13], a2[1:13]) {
		t.Error("nonce reused")
	}
}

func TestOpenRejects(t *testing.T) {
	b := newBox(t, 1)
	blob, err := b.Seal([]byte("pw"), []byte("camera:1"))
	if err != nil {
		t.Fatal(err)
	}
	flip := func(i int) []byte {
		c := bytes.Clone(blob)
		c[i] ^= 1
		return c
	}
	tests := []struct {
		name string
		box  *sealbox.Box
		blob []byte
		aad  string
		want error
	}{
		{"wrong aad", b, blob, "camera:2", sealbox.ErrOpen},
		{"no aad", b, blob, "", sealbox.ErrOpen},
		{"wrong key", newBox(t, 2), blob, "camera:1", sealbox.ErrOpen},
		{"tampered nonce", b, flip(1), "camera:1", sealbox.ErrOpen},
		{"tampered ciphertext", b, flip(13), "camera:1", sealbox.ErrOpen},
		{"tampered tag", b, flip(len(blob) - 1), "camera:1", sealbox.ErrOpen},
		{"truncated", b, blob[:20], "camera:1", sealbox.ErrOpen},
		{"empty", b, nil, "camera:1", sealbox.ErrOpen},
		{"unknown version", b, append([]byte{2}, blob[1:]...), "camera:1", sealbox.ErrUnsupportedVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := tt.box.Open(tt.blob, []byte(tt.aad)); !errors.Is(err, tt.want) || got != nil {
				t.Errorf("Open = %q, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := sealbox.New(make([]byte, n)); err == nil {
			t.Errorf("New(%d bytes) succeeded", n)
		}
	}
}
