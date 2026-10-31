package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fastParams keeps tests quick; production uses DefaultParams.
var fastParams = Params{MemoryKiB: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestHashAndVerify(t *testing.T) {
	h := NewHasher(fastParams, 2, time.Second)
	ctx := t.Context()
	enc, err := h.Hash(ctx, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Errorf("unexpected encoding %q", enc)
	}
	enc2, _ := h.Hash(ctx, "correct horse battery staple")
	if enc == enc2 {
		t.Error("two hashes of the same password are equal (salt not random)")
	}

	ok, rehash, err := h.Verify(ctx, enc, "correct horse battery staple")
	if err != nil || !ok || rehash {
		t.Errorf("Verify(correct) = %v, %v, %v", ok, rehash, err)
	}
	ok, _, err = h.Verify(ctx, enc, "correct horse battery stapleX")
	if err != nil || ok {
		t.Errorf("Verify(wrong) = %v, %v", ok, err)
	}
}

func TestVerifyDetectsRehash(t *testing.T) {
	old := NewHasher(fastParams, 1, time.Second)
	enc, err := old.Hash(t.Context(), "password-password")
	if err != nil {
		t.Fatal(err)
	}
	newer := fastParams
	newer.Time = 2
	ok, rehash, err := NewHasher(newer, 1, time.Second).Verify(t.Context(), enc, "password-password")
	if err != nil || !ok || !rehash {
		t.Errorf("Verify with new params = %v, %v, %v; want match and rehash", ok, rehash, err)
	}
	// A mismatch never asks for a rehash.
	ok, rehash, _ = NewHasher(newer, 1, time.Second).Verify(t.Context(), enc, "nope")
	if ok || rehash {
		t.Errorf("wrong password: ok=%v rehash=%v", ok, rehash)
	}
}

func TestVerifyKnownVector(t *testing.T) {
	// Produced by argon2-cffi 23.1.0 (libargon2), an independent implementation:
	// hash_secret(b"password", b"somesaltsomesalt", time_cost=2, memory_cost=64,
	//             parallelism=1, hash_len=32, type=Type.ID)
	const enc = "$argon2id$v=19$m=64,t=2,p=1$c29tZXNhbHRzb21lc2FsdA$Gpj7qOY5RCXJvcMzqcdQqvgR3wcPX7SleI4c9NtXk6E"
	h := NewHasher(fastParams, 1, time.Second)
	ok, rehash, err := h.Verify(t.Context(), enc, "password")
	if err != nil || !ok {
		t.Fatalf("Verify(reference vector) = %v, %v", ok, err)
	}
	if !rehash {
		t.Error("t=2 differs from the hasher's t=1, want rehash")
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	valid := "$argon2id$v=19$m=64,t=1,p=1$c29tZXNhbHRzYWx0$" + strings.Repeat("A", 43)
	if _, _, _, err := decodePHC(valid); err != nil {
		t.Fatalf("valid hash rejected: %v", err)
	}
	for _, s := range []string{
		"",
		"plain",
		"$argon2i$v=19$m=64,t=1,p=1$c29tZXNhbHRzYWx0$" + strings.Repeat("A", 43),
		"$argon2id$v=16$m=64,t=1,p=1$c29tZXNhbHRzYWx0$" + strings.Repeat("A", 43),
		"$argon2id$v=19$t=1,m=64,p=1$c29tZXNhbHRzYWx0$" + strings.Repeat("A", 43),
		"$argon2id$v=19$m=64,t=1$c29tZXNhbHRzYWx0$" + strings.Repeat("A", 43),
		"$argon2id$v=19$m=64,t=0,p=1$c29tZXNhbHRzYWx0$" + strings.Repeat("A", 43),
		"$argon2id$v=19$m=64,t=99,p=1$c29tZXNhbHRzYWx0$" + strings.Repeat("A", 43),
		"$argon2id$v=19$m=99999999,t=1,p=1$c29tZXNhbHRzYWx0$" + strings.Repeat("A", 43),
		"$argon2id$v=19$m=64,t=1,p=0$c29tZXNhbHRzYWx0$" + strings.Repeat("A", 43),
		"$argon2id$v=19$m=64,t=1,p=300$c29tZXNhbHRzYWx0$" + strings.Repeat("A", 43),
		"$argon2id$v=19$m=x,t=1,p=1$c29tZXNhbHRzYWx0$" + strings.Repeat("A", 43),
		"$argon2id$v=19$m=64,t=1,p=1$c2E$" + strings.Repeat("A", 43),               // salt too short
		"$argon2id$v=19$m=64,t=1,p=1$c29tZXNhbHRzYWx0$AAAA",                        // hash too short
		"$argon2id$v=19$m=64,t=1,p=1$c29tZXNhbHRzYWx0=$" + strings.Repeat("A", 43), // padding
		"$argon2id$v=19$m=64,t=1,p=1$c29tZXNhbHRzYWx0$" + strings.Repeat("A", 43) + "$x",
	} {
		if _, _, _, err := decodePHC(s); !errors.Is(err, errMalformedHash) {
			t.Errorf("decodePHC(%q) = %v, want errMalformedHash", s, err)
		}
	}
	if _, _, err := NewHasher(fastParams, 1, time.Second).Verify(t.Context(), "garbage", "x"); err == nil {
		t.Error("Verify accepted a malformed hash")
	}
}

func TestHasherLimitsConcurrency(t *testing.T) {
	h := NewHasher(fastParams, 1, 20*time.Millisecond)
	if err := h.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Hash(t.Context(), "x"); !errors.Is(err, ErrBusy) {
		t.Errorf("Hash while full = %v, want ErrBusy", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h2 := NewHasher(fastParams, 1, time.Hour)
	_ = h2.acquire(t.Context())
	if _, _, err := h2.Verify(ctx, "$argon2id$v=19$m=64,t=1,p=1$c29tZXNhbHRzYWx0$"+strings.Repeat("A", 43), "x"); !errors.Is(err, ErrBusy) {
		t.Errorf("Verify with cancelled context = %v, want ErrBusy", err)
	}
	h.release()
	if _, err := h.Hash(t.Context(), "x"); err != nil {
		t.Errorf("Hash after release = %v", err)
	}
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		pw string
		ok bool
	}{
		{strings.Repeat("a", 11), false},
		{strings.Repeat("a", 12), true},
		{strings.Repeat("a", 128), true},
		{strings.Repeat("a", 129), false},
		{strings.Repeat("あ", 12), true},  // 12 code points, 36 bytes
		{strings.Repeat("あ", 11), false}, // counted in code points, not bytes
		{strings.Repeat("あ", 128), true},
		{"change_me-please-1234", false},
		{"CHANGE_ME", false},
		{"short\xff\xfe\xfdpadding", false}, // invalid UTF-8
	}
	for _, tt := range tests {
		err := ValidatePassword(tt.pw)
		if (err == nil) != tt.ok {
			t.Errorf("ValidatePassword(%q) = %v, want ok=%v", tt.pw, err, tt.ok)
		}
		if err != nil && strings.Contains(err.Error(), tt.pw) {
			t.Errorf("error leaks the password: %v", err)
		}
	}
}

func TestValidateUsername(t *testing.T) {
	for name, ok := range map[string]bool{
		"admin": true, "A.b_c-9": true, strings.Repeat("a", 64): true,
		"": false, strings.Repeat("a", 65): false, "a b": false, "ユーザー": false, "a@b": false,
	} {
		if err := ValidateUsername(name); (err == nil) != ok {
			t.Errorf("ValidateUsername(%q) = %v, want ok=%v", name, err, ok)
		}
	}
}
