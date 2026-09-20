-- Payments queries (M5, CLAUDE.md §7, ADR 0008). The concurrency design in
-- one sentence: the SCHEMA is the arbiter — inserts collide on
-- payments_active_session_unique, captures are guarded conditional UPDATEs,
-- and the confirm gate is an EXISTS. No read-modify-write anywhere.

-- name: InsertPaymentIntent :one
-- Records a freshly gateway-minted intent. 23505 on
-- payments_active_session_unique means a concurrent CreateIntent won the
-- race; the repo maps that to ErrPaymentIntentExists and the service
-- recovers by returning the winner (same shape as booking confirm's
-- idempotent recovery, ADR 0006). 23505 on payments_intent_unique would be
-- a gateway ID collision — impossible by construction (UUIDs); the repo
-- logs it loudly rather than guessing.
INSERT INTO payments (booking_session_id, gateway, intent_id, status, amount_cents, currency)
VALUES ($1, $2, $3, 'intent', $4, $5)
RETURNING *;

-- name: GetActivePaymentBySession :one
-- The session's live intent: latest non-terminal row. The status predicate
-- mirrors payments_active_session_unique EXACTLY (migration 00009) so the
-- read and the arbiter can never disagree about what "active" means. The id
-- tiebreak exists because created_at is now()-stamped and two rows inserted
-- in one transaction instant would otherwise tie.
SELECT * FROM payments
WHERE booking_session_id = $1
  AND status NOT IN ('failed', 'refunded')
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetPaymentByIntent :one
-- Intent lookup by the gateway's ID (payments_intent_unique). This is the
-- webhook's entry point: the provider names an intent, we find our row.
SELECT * FROM payments WHERE intent_id = $1;

-- name: CapturePayment :one
-- The guarded capture: flips intent/authorized → captured and returns the
-- row, or returns ZERO rows if the payment is missing or already terminal.
-- The WHERE clause IS the state machine edge (domain/payment
-- CanTransitionTo) expressed as SQL, which makes a duplicate webhook
-- physically incapable of double-applying: the losing UPDATE blocks on the
-- row lock, re-evaluates the predicate, matches nothing. Zero rows is not
-- an error by itself — the repo then SELECTs by intent to distinguish
-- not-found / already-captured (idempotent no-op) / invalid transition.
UPDATE payments
SET status = 'captured', updated_at = now()
WHERE intent_id = $1 AND status IN ('intent', 'authorized')
RETURNING *;

-- name: FailPayment :one
-- Same guard shape as CapturePayment for the intent/authorized → failed
-- edge: gateway declines and webhook amount mismatches land here. A
-- captured row can NEVER become failed (money that moved exits only via
-- refund), and the guard makes that true at the DB level, not just in the
-- domain table. Zero rows → the repo SELECTs to classify, same as capture.
UPDATE payments
SET status = 'failed', updated_at = now()
WHERE intent_id = $1 AND status IN ('intent', 'authorized')
RETURNING *;

-- name: ExistsCapturedPaymentForSession :one
-- The booking confirm gate (application/booking.PaymentFinder): has the
-- money for this session moved? First-captured-wins — with at most one
-- active intent per session (00009) there is no ordering ambiguity, and
-- EXISTS short-circuits on the payments_session_idx index.
SELECT EXISTS (
    SELECT 1 FROM payments
    WHERE booking_session_id = $1 AND status = 'captured'
);
