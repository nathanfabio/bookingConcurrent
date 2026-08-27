package auth

import (
	"strings"
	"testing"
)

// cheapParams keeps the unit tests fast: argon2 at production parameters
// costs ~0.2-0.5 s per hash, which would make the suite miserable. The
// parameters are still valid (m >= 8*p) and exercise the exact same code
// paths — only the cost differs (ADR 0005).
var cheapParams = Argon2Params{
	MemoryKiB:   64,
	Time:        1,
	Parallelism: 1,
	KeyLen:      32,
	SaltLen:     16,
}

func TestArgon2HashVerifyRoundTrip(t *testing.T) {
	h := NewArgon2Hasher(cheapParams)

	hash, err := h.Hash("correct-horse-battery")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	// PHC shape: $argon2id$v=19$m=...,t=...,p=...$salt$hash
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Errorf("hash %q lacks the argon2id PHC prefix", hash)
	}
	if strings.Count(hash, "$") != 5 {
		t.Errorf("hash %q has %d '$' separators, want 5 (PHC format)", hash, strings.Count(hash, "$"))
	}

	ok, err := h.Verify(hash, "correct-horse-battery")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Errorf("Verify(correct password) = false, want true")
	}

	ok, err = h.Verify(hash, "wrong password")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Errorf("Verify(wrong password) = true, want false")
	}
}

// TestArgon2HashesAreSalted: two hashes of the same password must differ —
// a fresh salt per hash is what defeats rainbow-table reuse.
func TestArgon2HashesAreSalted(t *testing.T) {
	h := NewArgon2Hasher(cheapParams)
	h1, err := h.Hash("same-password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	h2, err := h.Hash("same-password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if h1 == h2 {
		t.Errorf("two hashes of the same password are identical; salt missing?")
	}
}

// TestArgon2VerifyIsSelfDescribing: Verify reads the parameters OUT OF the
// hash, so a hash produced under one parameter set verifies under a hasher
// configured differently. This is the property that lets us retune
// DefaultArgon2Params without a data migration (ADR 0005).
func TestArgon2VerifyIsSelfDescribing(t *testing.T) {
	writer := NewArgon2Hasher(cheapParams)
	hash, err := writer.Hash("old-parameter-hash")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	// A hasher configured with DIFFERENT parameters must still verify it.
	other := NewArgon2Hasher(Argon2Params{
		MemoryKiB:   128,
		Time:        2,
		Parallelism: 1,
		KeyLen:      32,
		SaltLen:     16,
	})
	ok, err := other.Verify(hash, "old-parameter-hash")
	if err != nil {
		t.Fatalf("Verify under different params: %v", err)
	}
	if !ok {
		t.Errorf("hash written under old params failed to verify; Verify must read embedded params")
	}
}

// TestArgon2VerifyRejectsForeignFormats: anything that is not an argon2id
// PHC string simply does not match — no error, just false. (bcrypt
// strings, argon2i/d variants, garbage.)
func TestArgon2VerifyRejectsForeignFormats(t *testing.T) {
	h := NewArgon2Hasher(cheapParams)
	cases := map[string]string{
		"bcrypt-style": "$2a$12$abcdefghijklmnopqrstuv",
		"argon2i":      "$argon2i$v=19$m=64,t=1,p=1$c2FsdA$aGFzaA",
		"argon2d":      "$argon2d$v=19$m=64,t=1,p=1$c2FsdA$aGFzaA",
		"garbage":      "not-a-hash",
		"empty":        "",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := h.Verify(input, "any-password")
			if err != nil {
				t.Errorf("Verify(%q) = error %v, want (false, nil) for non-argon2id input", input, err)
			}
			if ok {
				t.Errorf("Verify(%q) = true, want false", input)
			}
		})
	}
}

func TestArgon2NewHasherRejectsInvalidParams(t *testing.T) {
	cases := map[string]Argon2Params{
		"memory below 8*p": {MemoryKiB: 8, Time: 1, Parallelism: 2, KeyLen: 32, SaltLen: 16},
		"zero time":        {MemoryKiB: 64, Time: 0, Parallelism: 1, KeyLen: 32, SaltLen: 16},
		"zero parallelism": {MemoryKiB: 64, Time: 1, Parallelism: 0, KeyLen: 32, SaltLen: 16},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("NewArgon2Hasher(%+v) did not panic on invalid params", p)
				}
			}()
			NewArgon2Hasher(p)
		})
	}
}
