# 5. Password hashing: argon2id with explicit, reasoned parameters

Date: 2026-08-27
Status: Accepted

## Context

Passwords must be stored so a database leak does not become a credential
leak. CLAUDE.md §3 allows either bcrypt or argon2id and requires the choice
and its cost parameters to be written down, because the cost factor is
itself a security/performance trade-off worth reasoning about explicitly.

## Decision

**Use argon2id** (`golang.org/x/crypto/argon2`) **with these parameters:**

| Param | Value | What it controls |
|-------|-------|------------------|
| variant | `argon2id` | Hybrid of argon2i and argon2d; the OWASP-recommended variant |
| version | `v=19` | Current spec version, pinned in every hash |
| memory `m` | **64 MiB (65536 KiB)** | The anti-GPU/ASIC knob (below) |
| iterations `t` | **3** | Sequential passes over the memory |
| parallelism `p` | **2** | Lanes computed in parallel |
| key length | **32 bytes** | Output size; 256-bit security level |
| salt length | **16 bytes** | Fresh `crypto/rand` salt per password |

Why argon2id over bcrypt: bcrypt is CPU-hard but *memory-light* (a few KB),
which lets attackers parallelize it across thousands of GPU/ASIC cores
cheaply. Argon2id is **memory-hard** — every guess must allocate and churn a
large block of fast memory, and parallel attack hardware cannot amortize
memory the way it amortizes raw computation. That makes large-scale brute
force meaningfully more expensive. Argon2 was also the Password Hashing
Competition winner and is OWASP's first recommendation.

Why these values, balancing login latency against brute-force resistance:

- **Memory is the knob that matters most.** 64 MiB is far above OWASP's
  floor (19 MiB) and forces a real per-guess memory cost; it is still small
  enough that a login doesn't pressure the server. Memory dominates the
  defense; time and parallelism are secondary multipliers.
- **t=3** makes each guess do three sequential passes, tripling wall-time
  per guess without changing the memory footprint.
- **p=2** fills a couple of cores per hash. Going higher buys little defense
  but costs a thread per concurrent login, and logins are user-paced, so the
  server-side throughput concern is small — but not zero, hence 2 not 8.
- Together this lands one hash at roughly **0.2–0.5 s** on commodity
  hardware. That is the budget: slow enough that a billion guesses is
  infeasible, fast enough that a human clicking "log in" doesn't notice.

**Hashes are stored as PHC strings**
(`$argon2id$v=19$m=…,t=…,p=…$<salt>$<hash>`). The string is
*self-describing*: `Verify` reads the parameters back out of the stored hash
rather than using the configured ones. This is the property that lets us
retune `DefaultArgon2Params` later without a data migration — old hashes
keep verifying, and users are silently re-hashed at new parameters on their
next successful login if we ever add that path.

**Timing equalization**: login against an unknown email still runs one
argon2 verify (against a dummy hash computed at startup), and both failure
paths return the same sentinel, so response latency and body cannot
enumerate which emails are registered.

## Rejected alternative: bcrypt

bcrypt is a reasonable, battle-tested choice — the right answer for teams
that want a one-line API (`GenerateFromPassword`) with less parameter-tuning
risk, since its single cost factor is harder to get wrong than argon2's
three knobs. We chose argon2id because memory-hardness is a real defense
upgrade against modern hardware and this project values the explicit
trade-off. This is a preference, not a correction: bcrypt at cost ≥ 12 would
also have satisfied CLAUDE.md §3.

## Consequences

- `DefaultArgon2Params` lives in `internal/application/auth/hasher.go` with
  the reasoning above mirrored in code comments; tests override it with
  cheap parameters so the suite doesn't pay 0.5 s per hash.
- Logins are deliberately slow; rate limiting on `/auth/login` (a later
  milestone) protects the endpoint from being turned into a self-DoS.
- The `users.password_hash` column comment in `migrations/00001` says
  "bcrypt output." That migration is already applied, so the comment stays,
  but the column actually holds argon2id PHC strings. Both formats are
  self-describing, which is the property the comment is really about.
- Password input is capped at 128 characters: argon2 cost grows with input
  length, so the cap doubles as a DoS bound.
