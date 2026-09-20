# 8. Payments: fake sandbox gateway, webhook capture, capture-gated confirm

Date: 2026-09-20
Status: Accepted

## Context

CLAUDE.md §7 defines the payment flow: *hold → create payment intent →
(webhook or client confirms) → payment confirmed → booking confirmed in
Postgres → `BookingConfirmed` event published*. M4 built everything after
the arrow "booking confirmed" and left two explicit hooks for this
milestone: `Service.Confirm`'s comment ("M5 re-anchors this to payment
capture") and ADR 0006's race-window note ("M5 must re-anchor this to
payment capture"). The schema has been waiting since M2: migration 00005
created `payments` with a status vocabulary (`intent`, `authorized`,
`captured`, `refunding`, `refunded`, `failed`), a unique `intent_id`, a
non-unique `booking_session_id`, and the comment that status transitions
are "enforced in the domain (internal/domain/payment), not just by the
CHECK here".

Two constraints shaped the design. First, CLAUDE.md §7 forbids hitting a
real payment gateway from tests or CI and allows a clearly-labeled
fake/sandbox adapter when no real account is available; §13 forbids
gold-plating into a billing system. Second, ADR 0001's consumer-owned
ports rule means the gateway interface is defined by the use cases that
call it, and its justification is "two real implementations: fake sandbox
and Stripe" — the fake now, Stripe deferred until it can be exercised
against real test-mode keys.

## Decision

**1. One gateway implementation: a clearly-labeled in-process sandbox.**
`internal/adapters/payment.FakeGateway` implements the
`apppayment.PaymentGateway` port (`Provider`, `CreateIntent`,
`CaptureIntent`, `VerifyWebhook`). It lives in `adapters/payment`, NOT
`adapters/memory`: memory holds test doubles for our own stores, while the
fake gateway is a stand-in for an external provider that ships inside the
production binary when `PAYMENT_PROVIDER=fake` (the dev/CI default). It is
stateless — the `payments` table is the only durable state — physically
offline (the package imports no `net/*` or driver), and mints recognizable
`pi_fake_<uuid>` intent IDs so sandbox money is never mistaken for real.
Declines and outages are injected one-shot failures (`FailNextCapture`),
the sandbox equivalent of Stripe's decline-card test tokens. Selecting
`PAYMENT_PROVIDER=stripe` fails at boot in the composition root with a
pointer to this ADR — before M5 it would have booted silently, unable to
charge anyone; config still validates Stripe's keys first, because config
checks shape and wiring checks capability.

**2. Webhook verification is a Stripe-shaped HMAC scheme behind the port.**
Header `Payment-Signature: t=<unix>,v1=<hex>`; the MAC is HMAC-SHA256 over
`"<t>.<rawBody>"` — the EXACT bytes received, never a re-encoding — with a
±300s replay tolerance and constant-time comparison. Every failure mode
(missing/malformed header, bad hex, stale timestamp, MAC mismatch,
unparseable body) collapses into ONE opaque `ErrWebhookInvalid` → 400
`webhook_invalid`: telling a forger which check failed is an oracle. The
scheme mirrors Stripe's `Stripe-Signature` in essence, so the deferred
adapter parallels it 1:1 (`webhook.ConstructEvent` does the same job).
`VerifyWebhook` is a gateway method, not shared middleware: signature
formats are per-provider, and the port normalizes them into
`WebhookEvent{Kind, IntentID, AmountCents}`. The signing secret is a real
env-driven credential (`PAYMENT_FAKE_WEBHOOK_SECRET`, validated at boot,
documented in `.env.example`) — whoever holds it can forge captures, which
is exactly the lesson. The header NAME is duplicated as a private constant
in `adapters/http` because sibling adapters must not import each other
(lint-guards Guard 3); docs/api.md pins the wire contract.

**3. Confirm is gated on captured money — the M4 re-anchor.**
`booking.Service.Confirm` now runs: ownership → hold liveness (ADR 0006's
hard NO, unchanged) → `PaymentFinder.HasCapturedPayment` → commit. The
gate is a one-method consumer-owned port in `application/booking`
(`HasCapturedPayment`), satisfied by the same `PaymentRepo` and memory
fake that implement `apppayment.PaymentStore` — the two-small-interfaces
trade-off ADR 0001 already accepted for `ScreeningRepo`. Missing capture
answers **402 `payment_required`** (409 was the alternative; 402 is what
RFC 9110 reserves and what Stripe-style APIs use for exactly this, and
clients branch on the `code` regardless).

The gate deliberately reads OUTSIDE the booking+outbox transaction. In M5
capture is monotonic — nothing un-captures — so the worst interleaving is
answering `payment_required` a microsecond before the capture lands, and
the client's retry succeeds. Folding the check into the transaction would
buy no correctness here and would have to be re-evaluated the day refunds
(an un-capture) exist; that re-evaluation is recorded as a consequence,
not left implicit.

**4. The schema arbitrates payment races, exactly as it arbitrates booking
races (ADR 0006).**
- *One active intent per session*: migration 00009 adds the partial unique
  index `payments_active_session_unique ON payments (booking_session_id)
  WHERE status NOT IN ('failed','refunded')`. This REFINES 00005's
  "superseding retry abandons the previous intent" comment into an
  enforceable rule: supersede = fail the old row, then insert. Without it,
  a double-clicked pay button leaves two live intents and the gate's
  EXISTS read cannot tell which charge the buyer meant. `refunding`
  deliberately still blocks — a refund in flight must not race a fresh
  charge. Concurrent `CreateIntent` losers get 23505 →
  `ErrPaymentIntentExists` and recover by reading the winner back (the
  booking-confirm recovery shape). The loser's gateway-minted intent is
  abandoned — harmless for the stateless fake; voiding it is a
  Stripe-milestone task.
- *Double capture is physically impossible*: capture is ONE guarded UPDATE
  (`WHERE status IN ('intent','authorized')`). A duplicate webhook's
  UPDATE blocks on the row lock, re-evaluates, matches zero rows; the
  follow-up SELECT classifies not-found / already-captured (idempotent
  no-op, `capturedNow=false`) / invalid transition. `captured → failed` is
  unreachable in SQL and in the domain table alike: money that moved exits
  only via refund.
- *First captured wins*: the gate is `EXISTS(status='captured')` — with at
  most one active intent per session there is no ordering ambiguity.

**5. Prices freeze at intent creation.** Migration 00008 adds
`screenings.price_cents` (NOT NULL, CHECK > 0, default dropped after
backfill — no silent implicit prices, the schema echo of config's
fail-fast rule). `CreateIntent` reads the screening's price and freezes it
into `payments.amount_cents`; nothing recomputes it later. Screenings
deliberately have no currency column: `payment.DefaultCurrency = "usd"`
everywhere, because multi-currency implies FX semantics §13 forbids.
Webhook events carry the provider's amount, and the service cross-checks
it against the frozen value — provider numbers are input, not truth
(Stripe's own guidance). A mismatch fails the payment, logs loudly, and
acks: retrying cannot change the outcome.

**6. The webhook records provider truth; the client path protects the
buyer.** `HandleWebhook` never consults the hold: a capture arriving after
hold expiry is recorded as captured (200 ack), while `Confirm`'s expiry
hard-NO keeps the seat unsellable for that session — ADR 0006's "hard NO
even with payment authorized", now pinned with money actually moving
(`TestBookingLifecycleExpiry`). The captured row is the audit trail; the
refund is deferred operational work. The client-confirm path
(`POST /holds/{sessionID}/payment-intent/confirm` — the sandbox
equivalent of Stripe's client-side `paymentIntent.confirm()`, and the dev
trigger for "the buyer paid") checks hold liveness BEFORE calling the
gateway, so our own API never charges an expired hold. There is no
dev-only capture backdoor: the production-shaped path IS the fast path.

**7. Ack policy: every signature-valid event answers 200.** Duplicates,
unknown intents, unhandled kinds (forward compatibility — providers add
event types), and amount mismatches all ack with `{"received":true}` and
log loudly server-side; `WebhookOutcome.Applied` distinguishes "changed
state" from "acked inert" for logs and future metrics. Non-2xx answers
invite provider retry storms for outcomes that cannot change. Only
verification failure (400) and infrastructure errors (500) propagate.

**8. Deferred, explicitly** (§13): the Stripe test-mode adapter; refunds
(`refunding`/`refunded` are modeled in the domain table and CHECK but
unreachable in M5 flows, and `authorized` with them — the fake captures in
one step); automatic refund for the capture-after-expiry window (manual
ops, the payment row is the audit trail); voiding abandoned intents from
lost `CreateIntent` races; payment metrics (M8 owns counters).

## Consequences

- **New race window, safe direction**: hold expires between the client
  gate and the gateway capture → captured-but-unconfirmable. The money
  moved, the seat stays unsold, the row records it, and ops refunds
  manually until the refunds milestone. Every alternative (charging after
  expiry, or selling the seat anyway) violates a harder rule.
- **The monotonicity assumption is load-bearing.** Gate-outside-transaction
  and the two-statement capture classification are both safe ONLY because
  nothing un-captures in M5. Introducing refunds must re-evaluate both —
  this ADR is the reminder, and `CanTransitionTo`'s
  `captured → refunding` edge is where the re-evaluation starts.
- **402 joins the error vocabulary** (`payment_required`,
  `payment_declined`, `webhook_invalid`): distinct codes because recovery
  differs — required means "start checkout", declined means "the gateway
  refused; a superseding intent is already possible".
- **The no-probing rule extends to payment routes**: unknown, foreign,
  and expired sessions answer byte-identical 404s on
  `/payment-intent` and `/payment-intent/confirm` too
  (`TestPaymentIntentHandlerSessionProbingByteIdentical`), so the payment
  URL shape cannot enumerate sessions any better than the booking one.
- **Constraint names are schema dependencies again**: renaming
  `payments_active_session_unique` changes conflict semantics, so
  `TestPaymentRepoActiveSessionUniquePinned` asserts the literal name
  against the live database — a rename breaks the build, not production.
- **Card data never exists to leak**: the sandbox has none, the table
  stores only intent ID/status/amount (§7), and logs carry `intent_id`,
  `event_kind`, and amounts — never signature values, never raw bodies,
  never the secret (lint-guards Guard 2 watches the log keys).
- **Tests that prove this model**: domain transition table (exhaustive
  6×6); fake-store guard semantics; service races
  (`TestServiceConcurrentWebhookAndClientConfirmOneCapture`,
  `TestServiceConcurrentCaptureConfirmExactlyOneBooking`); repo races
  against live Postgres
  (`TestPaymentRepoConcurrentCaptureExactlyOneTransition`, 32 goroutines,
  exactly one transition); webhook forgery suite (tamper, wrong secret,
  stale timestamp, malformed header — one opaque 400); duplicate-webhook
  no-op end-to-end; capture-after-expiry honest bookkeeping; the full
  lifecycle (hold → unpaid confirm 402s → intent → signed webhook →
  confirm → exactly one pending `BookingConfirmed` outbox row).
