// Package payment holds the payment gateway adapters (CLAUDE.md §1's
// adapters/payment, §7's gateway integration).
//
// M5 ships exactly one: FakeGateway, a clearly-labeled in-process SANDBOX.
// It lives here and not in adapters/memory on purpose — memory is the home
// of test doubles standing in for our OWN stores, while the fake gateway is
// a stand-in for an external provider, wired into the production binary
// when PAYMENT_PROVIDER=fake (the dev and CI default). ADR 0008 records the
// split and the deferred Stripe adapter.
//
// The sandbox is physically incapable of talking anywhere: this package
// imports no net/*, no driver, no HTTP client. Capture is a pure in-process
// decision, and webhook "delivery" is a signing helper tests and dev
// scripts call by hand.
package payment

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	apppayment "github.com/nathanfabio/bookingConcurrent/internal/application/payment"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/payment"
)

// Compile-time proof the sandbox satisfies the port.
var _ apppayment.PaymentGateway = (*FakeGateway)(nil)

// WebhookSignatureHeader carries the HMAC over the raw body. The name is
// part of the wire contract pinned by docs/api.md; adapters/http keeps its
// own private copy because sibling adapters must not import each other
// (scripts/lint-guards.sh Guard 3).
const WebhookSignatureHeader = "Payment-Signature"

// webhookTolerance is the replay window: a signature whose timestamp is
// further than this from the verifier's clock is rejected. Five minutes
// matches Stripe's default tolerance; the value is part of the scheme a
// future Stripe adapter parallels 1:1 (ADR 0008).
const webhookTolerance = 5 * time.Minute

// fakeIntentPrefix makes sandbox intent IDs instantly recognizable in
// payment rows, logs, and psql sessions — a captured payment whose intent
// starts with this prefix can never be mistaken for real money moving.
const fakeIntentPrefix = "pi_fake_"

// webhookBody is the fake provider's wire format. snake_case like every
// other JSON contract in this repo; the real provider's payload shape is an
// adapter-internal concern behind VerifyWebhook, which is exactly why the
// port normalizes to apppayment.WebhookEvent.
type webhookBody struct {
	Kind        apppayment.EventKind `json:"kind"`
	IntentID    string               `json:"intent_id"`
	AmountCents int                  `json:"amount_cents"`
}

// FakeGateway is the deterministic in-process sandbox implementing
// apppayment.PaymentGateway.
//
// Faithfulness notes (where it deliberately matches a real provider):
//   - It is STATELESS: it keeps no intent registry, because in the real
//     system the payments table is the only durable state and the gateway
//     is the authority on its own intents. CaptureIntent therefore always
//     succeeds unless a failure is injected — the "did this intent exist?"
//     question belongs to our store, mirroring how a real provider would
//     answer with its own records, not ours.
//   - VerifyWebhook implements the full scheme a production gateway uses:
//     timestamped HMAC-SHA256 over the EXACT received bytes, constant-time
//     compare, replay tolerance, and one opaque error for every failure
//     mode (domain.ErrWebhookInvalid) so a forger learns nothing about
//     which check failed.
//   - FailNextCreate/FailNextCapture are one-shot injected failures —
//     TEST-ONLY hooks for decline and outage paths, the sandbox equivalent
//     of Stripe's decline-card test tokens.
type FakeGateway struct {
	mu     sync.Mutex
	secret []byte

	failCreate  error // one-shot; consumed by the next CreateIntent
	failCapture error // one-shot; consumed by the next CaptureIntent
}

// NewFakeGateway builds the sandbox with the webhook signing secret from
// validated config (PAYMENT_FAKE_WEBHOOK_SECRET). An empty secret is a
// configuration bug, not a runtime condition: fail at wiring time.
func NewFakeGateway(webhookSecret string) (*FakeGateway, error) {
	if webhookSecret == "" {
		return nil, errors.New("payment: fake gateway requires a webhook secret (PAYMENT_FAKE_WEBHOOK_SECRET)")
	}
	return &FakeGateway{secret: []byte(webhookSecret)}, nil
}

// Provider implements apppayment.PaymentGateway. The value is stamped into
// payments.gateway and must stay inside migration 00005's CHECK vocabulary.
func (f *FakeGateway) Provider() string { return "fake" }

// CreateIntent implements apppayment.PaymentGateway: mints a recognizable,
// collision-free intent ID without keeping state.
func (f *FakeGateway) CreateIntent(ctx context.Context, req apppayment.IntentRequest) (apppayment.Intent, error) {
	if err := f.takeFail(&f.failCreate); err != nil {
		return apppayment.Intent{}, err
	}
	if req.AmountCents <= 0 {
		// A real gateway rejects non-positive amounts; the sandbox mirrors
		// that instead of minting nonsense the payments CHECK would catch
		// later anyway (amount_cents >= 0, but business rule is > 0).
		return apppayment.Intent{}, fmt.Errorf("payment: fake gateway: amount must be positive, got %d", req.AmountCents)
	}
	return apppayment.Intent{ID: fakeIntentPrefix + uuid.NewString()}, nil
}

// CaptureIntent implements apppayment.PaymentGateway. The sandbox always
// collects unless a failure was injected — there is no card to decline.
func (f *FakeGateway) CaptureIntent(ctx context.Context, intentID string, amountCents int) error {
	return f.takeFail(&f.failCapture)
}

// VerifyWebhook implements apppayment.PaymentGateway. The signature covers
// "<t>.<rawBody>" — the EXACT bytes received, never a re-encoding — and
// every failure (malformed header, bad hex, unknown format, stale
// timestamp, MAC mismatch, unparseable or shapeless body) collapses into
// the single opaque domain.ErrWebhookInvalid.
func (f *FakeGateway) VerifyWebhook(rawBody []byte, signatureHeader string) (apppayment.WebhookEvent, error) {
	tStr, v1Hex, ok := parseSignatureHeader(signatureHeader)
	if !ok {
		return apppayment.WebhookEvent{}, domain.ErrWebhookInvalid
	}
	tUnix, err := strconv.ParseInt(tStr, 10, 64)
	if err != nil {
		return apppayment.WebhookEvent{}, domain.ErrWebhookInvalid
	}
	sigBytes, err := hex.DecodeString(v1Hex)
	if err != nil {
		return apppayment.WebhookEvent{}, domain.ErrWebhookInvalid
	}

	// Replay tolerance first: an expired signature is dead no matter how
	// well-formed, and checking the cheap claim before the MAC keeps the
	// failure modes from leaking order information through timing.
	signedAt := time.Unix(tUnix, 0)
	if d := time.Since(signedAt); d > webhookTolerance || d < -webhookTolerance {
		return apppayment.WebhookEvent{}, domain.ErrWebhookInvalid
	}

	mac := hmac.New(sha256.New, f.secret)
	mac.Write([]byte(tStr))
	mac.Write([]byte("."))
	mac.Write(rawBody)
	if !hmac.Equal(sigBytes, mac.Sum(nil)) { // constant-time, always
		return apppayment.WebhookEvent{}, domain.ErrWebhookInvalid
	}

	var body webhookBody
	dec := json.NewDecoder(strings.NewReader(string(rawBody)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return apppayment.WebhookEvent{}, domain.ErrWebhookInvalid
	}
	if dec.More() { // trailing garbage after a valid object
		return apppayment.WebhookEvent{}, domain.ErrWebhookInvalid
	}
	if body.IntentID == "" {
		return apppayment.WebhookEvent{}, domain.ErrWebhookInvalid
	}
	// Kind is NOT vocabulary-checked here: an unknown event kind is a
	// forward-compatible no-op the SERVICE acks and logs (providers add
	// event types; a signature-valid event we don't handle yet is not a
	// verification failure).
	return apppayment.WebhookEvent{
		Kind:        body.Kind,
		IntentID:    body.IntentID,
		AmountCents: body.AmountCents,
	}, nil
}

// SignWebhook renders a verified-shape webhook delivery: the exact body
// bytes plus the header VerifyWebhook accepts for them. It is fake-only
// export (NOT part of the port) — the sandbox equivalent of Stripe's CLI
// `trigger` command. Tests use it to drive the webhook path end-to-end;
// docs/api.md gives the openssl one-liner for manual dev runs. now is a
// parameter so tests can sign stale timestamps deliberately.
func (f *FakeGateway) SignWebhook(ev apppayment.WebhookEvent, now time.Time) (body []byte, header string) {
	body, err := json.Marshal(webhookBody{
		Kind:        ev.Kind,
		IntentID:    ev.IntentID,
		AmountCents: ev.AmountCents,
	})
	if err != nil {
		// webhookBody is three scalar fields; marshaling cannot fail. If it
		// ever does, a panic in a test helper beats a silently unsigned body.
		panic(fmt.Sprintf("payment: fake gateway: sign webhook: %v", err))
	}
	tStr := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, f.secret)
	mac.Write([]byte(tStr))
	mac.Write([]byte("."))
	mac.Write(body)
	return body, "t=" + tStr + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// FailNextCreate injects a one-shot CreateIntent failure (gateway outage
// path). TEST-ONLY, like FailNextCapture.
func (f *FakeGateway) FailNextCreate(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCreate = err
}

// FailNextCapture injects a one-shot CaptureIntent failure — the sandbox's
// declined card. The use case marks the payment failed and answers
// domain.ErrPaymentDeclined. TEST-ONLY.
func (f *FakeGateway) FailNextCapture(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCapture = err
}

// takeFail consumes a one-shot injected failure under the mutex.
func (f *FakeGateway) takeFail(slot *error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := *slot
	*slot = nil
	return err
}

// parseSignatureHeader splits "t=<unix>,v1=<hex>" into its parts. The
// format is Stripe's in essence (same field names, same comma separation),
// so the parsing discipline — reject anything that is not exactly the
// expected shape — is the discipline a Stripe adapter needs anyway.
func parseSignatureHeader(header string) (t, v1 string, ok bool) {
	var sawT, sawV1 bool
	for _, part := range strings.Split(header, ",") {
		key, value, found := strings.Cut(part, "=")
		if !found {
			return "", "", false
		}
		switch key {
		case "t":
			t, sawT = value, true
		case "v1":
			v1, sawV1 = value, true
		default:
			return "", "", false // unknown field: not our scheme
		}
	}
	if !sawT || !sawV1 || t == "" || v1 == "" {
		return "", "", false
	}
	return t, v1, true
}
