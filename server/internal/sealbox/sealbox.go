// Package sealbox encrypts small secrets for storage in the database with
// AES-256-GCM.
//
// A sealed blob is
//
//	version (1 byte) || nonce (12 bytes) || ciphertext || tag (16 bytes)
//
// The nonce is random for every Seal (96 bits from crypto/rand). The version
// byte names the key and format, so a later key rotation can tell old blobs
// from new ones. It is authenticated together with the caller's associated
// data (AAD): callers bind a blob to its row (for example the camera ID), so
// a blob copied into another row fails to open.
package sealbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// KeySize is the AES-256 key length.
const KeySize = 32

// Version1 is the current format: AES-256-GCM with the key from
// VMS_SECRET_KEY.
const Version1 byte = 1

const nonceSize = 12

// ErrOpen means the blob could not be decrypted: it was tampered with, was
// sealed with another key or for other associated data, or is malformed.
// The causes are deliberately not distinguished.
var ErrOpen = errors.New("sealbox: cannot decrypt (wrong key, wrong associated data, or corrupted)")

// ErrUnsupportedVersion means the blob was sealed in a format (or with a key
// version) this build does not know.
var ErrUnsupportedVersion = errors.New("sealbox: unsupported version")

// Box seals and opens blobs with one key.
type Box struct {
	aead cipher.AEAD
}

// New returns a Box for a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("sealbox: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("sealbox: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("sealbox: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext, binding it to aad.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	out := make([]byte, 1+nonceSize, 1+nonceSize+len(plaintext)+b.aead.Overhead())
	out[0] = Version1
	if _, err := rand.Read(out[1 : 1+nonceSize]); err != nil {
		return nil, fmt.Errorf("sealbox: read random nonce: %w", err)
	}
	return b.aead.Seal(out, out[1:1+nonceSize], plaintext, fullAAD(Version1, aad)), nil
}

// Open decrypts a blob made by Seal with the same aad.
func (b *Box) Open(blob, aad []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, ErrOpen
	}
	if blob[0] != Version1 {
		return nil, ErrUnsupportedVersion
	}
	if len(blob) < 1+nonceSize+b.aead.Overhead() {
		return nil, ErrOpen
	}
	nonce, ct := blob[1:1+nonceSize], blob[1+nonceSize:]
	pt, err := b.aead.Open(nil, nonce, ct, fullAAD(blob[0], aad))
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}

// fullAAD authenticates the version byte along with the caller's data, so
// the version cannot be changed without detection.
func fullAAD(version byte, aad []byte) []byte {
	out := make([]byte, 0, 1+len(aad))
	out = append(out, version)
	return append(out, aad...)
}
