package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2Params carries the tuning knobs for argon2id (ADR 0005). Every
// hash embeds the parameters that produced it (PHC format), so these
// values can change over time without invalidating stored hashes.
type Argon2Params struct {
	// MemoryKiB (m=) is the core anti-GPU/ASIC knob: each guess must
	// allocate this much fast memory, which attack hardware cannot
	// amortize across thousands of parallel cores the way it can pure
	// computation.
	MemoryKiB uint32
	// Time (t=) is the number of sequential passes over that memory; it
	// multiplies wall-time per guess.
	Time uint32
	// Parallelism (p=) is the number of lanes computed in parallel. Going
	// above 2 buys little defense but costs a thread per hash under login
	// load.
	Parallelism uint8
	// KeyLen is the derived-output length in bytes. 32 matches SHA-256's
	// security level; longer adds cost without defense.
	KeyLen uint32
	// SaltLen is the per-password random salt length in bytes. 16 (128
	// bits) defeats rainbow-table reuse across users and across systems.
	SaltLen uint32
}

// DefaultArgon2Params is the production choice (ADR 0005): 64 MiB / 3
// passes / 2 lanes lands a single hash around 0.2-0.5 s on commodity
// hardware — comfortable for an interactive login, punishing at a billion
// guesses. The OWASP Password Storage Cheat Sheet's floor for argon2id is
// 19 MiB / t=2 / p=1; we sit well above it because logins are rare,
// low-volume events and the latency budget is generous.
var DefaultArgon2Params = Argon2Params{
	MemoryKiB:   65536, // 64 MiB
	Time:        3,
	Parallelism: 2,
	KeyLen:      32,
	SaltLen:     16,
}

// test-friendly lower bound: argon2 requires m >= 8*p.
func (p Argon2Params) valid() error {
	if p.MemoryKiB < 8*uint32(p.Parallelism) {
		return fmt.Errorf("auth: argon2 memory %d KiB below minimum 8*p=%d", p.MemoryKiB, 8*uint32(p.Parallelism))
	}
	if p.Time < 1 || p.Parallelism < 1 || p.KeyLen < 1 || p.SaltLen < 1 {
		return errors.New("auth: argon2 parameters must all be positive")
	}
	return nil
}

// Argon2Hasher hashes and verifies passwords with argon2id (CLAUDE.md §3,
// ADR 0005). It is a concrete type, not an interface: there is exactly one
// implementation, and tests vary its behavior through cheap parameters
// rather than mocks.
type Argon2Hasher struct {
	params Argon2Params
}

// NewArgon2Hasher builds a hasher. It panics on invalid params: this is a
// boot-time construction error, not a request-time one, and a misconfigured
// hasher must never serve traffic (the config is validated at startup, but
// the invariant is restated here where it is enforced).
func NewArgon2Hasher(p Argon2Params) *Argon2Hasher {
	if err := p.valid(); err != nil {
		panic(err)
	}
	return &Argon2Hasher{params: p}
}

// argon2Version is the algorithm version byte defined by the reference
// implementation (0x13 = 19 decimal) and pinned in every PHC string.
const argon2Version = 19

// phcB64 is the PHC string encoding: standard alphabet, NO padding.
var phcB64 = base64.RawStdEncoding

// Hash returns the PHC-string encoding of password:
//
//	$argon2id$v=19$m=<memory>,t=<time>,p=<parallelism>$<salt>$<hash>
//
// The string is self-describing: Verify reads the parameters back out of
// it, which is what lets us change DefaultArgon2Params in the future
// without a data migration (ADR 0005). The salt is fresh crypto/rand
// bytes per call.
func (h *Argon2Hasher) Hash(password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		// crypto/rand failing means no functioning entropy source; no hash
		// we could invent would be safe (same stance as middleware.newUUID).
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.MemoryKiB, h.params.Parallelism, h.params.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2Version,
		h.params.MemoryKiB, h.params.Time, h.params.Parallelism,
		phcB64.EncodeToString(salt), phcB64.EncodeToString(key)), nil
}

// Verify reports whether password matches a stored PHC hash. It re-derives
// using the parameters EMBEDDED in the hash (not the hasher's configured
// ones), so hashes written under older parameters keep verifying after a
// parameter change. Comparison is constant-time.
//
// Non-argon2id input — bcrypt strings, argon2i/argon2d variants, garbage —
// returns (false, nil): it simply does not match. An error is returned
// only for argon2id strings that are structurally corrupt.
func (h *Argon2Hasher) Verify(hash, password string) (bool, error) {
	parts := strings.Split(hash, "$")
	// Splitting "$argon2id$v=19$m=...$salt$hash" yields
	// ["", "argon2id", "v=19", "m=...", "<salt>", "<hash>"].
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, nil
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2Version {
		return false, nil
	}
	var m, t uint32
	var p uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, fmt.Errorf("auth: parse argon2 parameters: %w", err)
	}
	if p > 255 {
		return false, fmt.Errorf("auth: argon2 parallelism %d out of range", p)
	}
	salt, err := phcB64.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("auth: decode argon2 salt: %w", err)
	}
	want, err := phcB64.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("auth: decode argon2 hash: %w", err)
	}
	got := argon2.IDKey([]byte(password), salt, t, m, uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
