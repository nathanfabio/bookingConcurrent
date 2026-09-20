package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	paymentadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/payment"
	apppayment "github.com/nathanfabio/bookingConcurrent/internal/application/payment"
)

// testWebhookSecret is a fixture, not a credential: it only ever signs
// webhooks this test file forges against the in-process fake gateway.
const testWebhookSecret = "unit-test-webhook-secret-not-real"

// newPaymentEnv wires the real payment Service over the shared in-memory
// fakes and the REAL fake gateway — handler tests exercise the production
// signing/verification code path, not a stub of it (the sandbox adapter is
// deterministic and offline, which is exactly what §9 asks of test deps).
func newPaymentEnv(t *testing.T) (*bookingEnv, *apppayment.Service, *paymentadapter.FakeGateway) {
	t.Helper()
	env := newBookingEnv(4)
	env.seedCatalog()
	gw, err := paymentadapter.NewFakeGateway(testWebhookSecret)
	if err != nil {
		t.Fatalf("fake gateway: %v", err)
	}
	svc := apppayment.NewService(gw, env.payments, env.holds, env.screenings, env.clock.Now)
	return env, svc, gw
}

// holdSession drives the real HoldHandler for alice on seat A1 and returns
// the session ID — the same checkout precondition every payment route has.
func holdSession(t *testing.T, env *bookingEnv) string {
	t.Helper()
	rec := httptest.NewRecorder()
	HoldHandler(env.svc).ServeHTTP(rec, reqAs(http.MethodPost, "/holds", holdBody("A", 1), aliceID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup hold: %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp holdResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("hold response: %v", err)
	}
	return resp.SessionID
}

func sessionPathReq(path, session, userID string) *http.Request {
	return pathReq(http.MethodPost, path, "", userID, map[string]string{"sessionID": session})
}

func TestPaymentIntentHandler(t *testing.T) {
	t.Run("first call is 201 with the frozen price", func(t *testing.T) {
		env, svc, _ := newPaymentEnv(t)
		session := holdSession(t, env)
		h := PaymentIntentHandler(svc)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, sessionPathReq("/holds/"+session+"/payment-intent", session, aliceID))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
		}
		var resp paymentIntentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if resp.PaymentID == "" || resp.SessionID != session {
			t.Errorf("payment/session = %q/%q, want non-empty/%q", resp.PaymentID, resp.SessionID, session)
		}
		if !strings.HasPrefix(resp.IntentID, "pi_fake_") {
			t.Errorf("intent_id = %q, want the recognizable sandbox prefix", resp.IntentID)
		}
		// The amount is the screening's price, frozen at creation.
		if resp.AmountCents != 1200 || resp.Currency != "usd" || resp.Status != "intent" {
			t.Errorf("amount/currency/status = %d/%s/%s, want 1200/usd/intent",
				resp.AmountCents, resp.Currency, resp.Status)
		}
		if !resp.HoldExpiresAt.Equal(bookingTestStart.Add(bookingTestTTL)) {
			t.Errorf("hold_expires_at = %v, want the hold's deadline", resp.HoldExpiresAt)
		}
	})

	t.Run("replay is 200 with the same intent", func(t *testing.T) {
		env, svc, _ := newPaymentEnv(t)
		session := holdSession(t, env)
		h := PaymentIntentHandler(svc)

		first := httptest.NewRecorder()
		h.ServeHTTP(first, sessionPathReq("/holds/"+session+"/payment-intent", session, aliceID))
		replay := httptest.NewRecorder()
		h.ServeHTTP(replay, sessionPathReq("/holds/"+session+"/payment-intent", session, aliceID))

		if replay.Code != http.StatusOK {
			t.Fatalf("replay status = %d, want 200; body: %s", replay.Code, replay.Body.String())
		}
		var a, b paymentIntentResponse
		if err := json.Unmarshal(first.Body.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(replay.Body.Bytes(), &b); err != nil {
			t.Fatal(err)
		}
		if a.IntentID != b.IntentID || a.PaymentID != b.PaymentID {
			t.Errorf("replay minted a second intent: %s vs %s", a.IntentID, b.IntentID)
		}
	})

	t.Run("unauthenticated wiring is a 401", func(t *testing.T) {
		_, svc, _ := newPaymentEnv(t)
		rec := httptest.NewRecorder()
		PaymentIntentHandler(svc).ServeHTTP(rec,
			pathReq(http.MethodPost, "/holds/x/payment-intent", "", "", map[string]string{"sessionID": "x"}))
		assertErrorResponse(t, rec, http.StatusUnauthorized, CodeUnauthorized)
	})
}

// TestPaymentIntentHandlerSessionProbingByteIdentical extends the §3
// no-probing rule to the payment routes: unknown, foreign, and expired
// sessions must be indistinguishable — same status, same code, same body
// bytes — so the payment URL shape cannot be used to enumerate sessions.
func TestPaymentIntentHandlerSessionProbingByteIdentical(t *testing.T) {
	env, svc, _ := newPaymentEnv(t)
	session := holdSession(t, env)
	h := PaymentIntentHandler(svc)

	cases := map[string]*httptest.ResponseRecorder{}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, sessionPathReq("/holds/no-such/payment-intent", "no-such", aliceID))
	cases["unknown session"] = rec

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, sessionPathReq("/holds/"+session+"/payment-intent", session, bobID))
	cases["someone else's session"] = rec

	env.clock.Advance(bookingTestTTL) // inclusive expiry
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, sessionPathReq("/holds/"+session+"/payment-intent", session, aliceID))
	cases["expired session"] = rec
	env.clock.Set(bookingTestStart)

	var reference string
	for name, r := range cases {
		assertErrorResponse(t, r, http.StatusNotFound, CodeNotFound)
		if reference == "" {
			reference = r.Body.String()
			continue
		}
		if r.Body.String() != reference {
			t.Errorf("%q body differs from the reference:\n%s\nvs\n%s", name, r.Body.String(), reference)
		}
	}
}

func TestPaymentConfirmHandler(t *testing.T) {
	intentFirst := func(t *testing.T, svc *apppayment.Service, session string) {
		t.Helper()
		rec := httptest.NewRecorder()
		PaymentIntentHandler(svc).ServeHTTP(rec, sessionPathReq("/holds/"+session+"/payment-intent", session, aliceID))
		if rec.Code != http.StatusCreated {
			t.Fatalf("setup intent: %d; body: %s", rec.Code, rec.Body.String())
		}
	}

	t.Run("client confirm captures", func(t *testing.T) {
		env, svc, _ := newPaymentEnv(t)
		session := holdSession(t, env)
		intentFirst(t, svc, session)

		rec := httptest.NewRecorder()
		PaymentConfirmHandler(svc).ServeHTTP(rec, sessionPathReq("/holds/"+session+"/payment-intent/confirm", session, aliceID))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		var resp paymentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if resp.Status != "captured" || resp.AmountCents != 1200 || resp.Currency != "usd" {
			t.Errorf("payment = %+v, want captured/1200/usd", resp)
		}

		// Replay is the same 200 — capture is idempotent.
		replay := httptest.NewRecorder()
		PaymentConfirmHandler(svc).ServeHTTP(replay, sessionPathReq("/holds/"+session+"/payment-intent/confirm", session, aliceID))
		if replay.Code != http.StatusOK {
			t.Errorf("replay status = %d, want 200; body: %s", replay.Code, replay.Body.String())
		}
	})

	t.Run("without an intent it is 402 payment_required", func(t *testing.T) {
		env, svc, _ := newPaymentEnv(t)
		session := holdSession(t, env)

		rec := httptest.NewRecorder()
		PaymentConfirmHandler(svc).ServeHTTP(rec, sessionPathReq("/holds/"+session+"/payment-intent/confirm", session, aliceID))
		assertErrorResponse(t, rec, http.StatusPaymentRequired, CodePaymentRequired)
	})

	t.Run("gateway decline is 402 payment_declined", func(t *testing.T) {
		env, svc, gw := newPaymentEnv(t)
		session := holdSession(t, env)
		intentFirst(t, svc, session)

		gw.FailNextCapture(errors.New("fake: card_declined"))
		rec := httptest.NewRecorder()
		PaymentConfirmHandler(svc).ServeHTTP(rec, sessionPathReq("/holds/"+session+"/payment-intent/confirm", session, aliceID))
		assertErrorResponse(t, rec, http.StatusPaymentRequired, CodePaymentDeclined)
	})
}

func TestPaymentWebhookHandler(t *testing.T) {
	// intentFor mints an intent through the real handler and returns its
	// id + frozen amount, the two things a provider event names.
	intentFor := func(t *testing.T, svc *apppayment.Service, session string) (string, int) {
		t.Helper()
		rec := httptest.NewRecorder()
		PaymentIntentHandler(svc).ServeHTTP(rec, sessionPathReq("/holds/"+session+"/payment-intent", session, aliceID))
		if rec.Code != http.StatusCreated {
			t.Fatalf("setup intent: %d; body: %s", rec.Code, rec.Body.String())
		}
		var resp paymentIntentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.IntentID, resp.AmountCents
	}

	t.Run("signed capture acks and opens the confirm gate", func(t *testing.T) {
		env, svc, gw := newPaymentEnv(t)
		session := holdSession(t, env)
		intent, amount := intentFor(t, svc, session)

		body, header := gw.SignWebhook(apppayment.WebhookEvent{
			Kind: apppayment.EventCaptured, IntentID: intent, AmountCents: amount,
		}, time.Now())
		rec := httptest.NewRecorder()
		PaymentWebhookHandler(svc).ServeHTTP(rec, webhookReq(body, header))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		var ack webhookAckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil || !ack.Received {
			t.Fatalf("ack = %s (err %v), want {\"received\":true}", rec.Body.String(), err)
		}

		// Duplicate delivery: still 200, still one capture (at-least-once
		// providers WILL redeliver).
		dup := httptest.NewRecorder()
		PaymentWebhookHandler(svc).ServeHTTP(dup, webhookReq(body, header))
		if dup.Code != http.StatusOK {
			t.Errorf("duplicate status = %d, want 200; body: %s", dup.Code, dup.Body.String())
		}

		// The booking confirm gate now passes end-to-end.
		confirm := httptest.NewRecorder()
		ConfirmHandler(env.svc).ServeHTTP(confirm, sessionPathReq("/holds/"+session+"/confirm", session, aliceID))
		if confirm.Code != http.StatusCreated {
			t.Errorf("confirm after webhook capture = %d, want 201; body: %s", confirm.Code, confirm.Body.String())
		}
	})

	t.Run("verification failures are one opaque 400", func(t *testing.T) {
		env, svc, gw := newPaymentEnv(t)
		session := holdSession(t, env)
		intent, amount := intentFor(t, svc, session)

		body, header := gw.SignWebhook(apppayment.WebhookEvent{
			Kind: apppayment.EventCaptured, IntentID: intent, AmountCents: amount,
		}, time.Now())

		cases := map[string]*http.Request{
			"missing header":   webhookReq(body, ""),
			"tampered body":    webhookReq(append(body, ' '), header),
			"wrong signature":  webhookReq(body, strings.Replace(header, "v1=", "v1=deadbeef", 1)),
			"stale timestamp":  mustStaleWebhook(t, gw, intent, amount),
			"malformed header": webhookReq(body, "nonsense"),
		}
		for name, req := range cases {
			rec := httptest.NewRecorder()
			PaymentWebhookHandler(svc).ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400; body: %s", name, rec.Code, rec.Body.String())
				continue
			}
			var errBody errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
				t.Fatalf("%s: error body not JSON: %v", name, err)
			}
			// One code, one message — a forger must not learn which check
			// failed (ADR 0008).
			if errBody.Code != CodeWebhookInvalid {
				t.Errorf("%s: code = %q, want %s", name, errBody.Code, CodeWebhookInvalid)
			}
		}
	})

	t.Run("unknown intent acks without applying", func(t *testing.T) {
		_, svc, gw := newPaymentEnv(t)
		body, header := gw.SignWebhook(apppayment.WebhookEvent{
			Kind: apppayment.EventCaptured, IntentID: "pi_fake_never-minted", AmountCents: 1200,
		}, time.Now())
		rec := httptest.NewRecorder()
		PaymentWebhookHandler(svc).ServeHTTP(rec, webhookReq(body, header))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 (ack stops provider retry storms)", rec.Code)
		}
	})
}

// TestConfirmHandlerRequiresPayment pins the M5 change to the M4 endpoint:
// a live, owned hold with no captured payment answers 402 payment_required,
// not 201.
func TestConfirmHandlerRequiresPayment(t *testing.T) {
	env, _, _ := newPaymentEnv(t)
	session := holdSession(t, env)

	rec := httptest.NewRecorder()
	ConfirmHandler(env.svc).ServeHTTP(rec, sessionPathReq("/holds/"+session+"/confirm", session, aliceID))
	assertErrorResponse(t, rec, http.StatusPaymentRequired, CodePaymentRequired)
}

// TestPaymentDTOsHaveSnakeCaseTags extends the §4 mechanical check to the
// payment DTOs.
func TestPaymentDTOsHaveSnakeCaseTags(t *testing.T) {
	dtos := []any{paymentIntentResponse{}, paymentResponse{}, webhookAckResponse{}}
	for _, dto := range dtos {
		typ := reflect.TypeOf(dto)
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag, ok := field.Tag.Lookup("json")
			if !ok {
				t.Errorf("%s.%s has no json tag", typ.Name(), field.Name)
				continue
			}
			name := strings.Split(tag, ",")[0]
			if name == "" || strings.ToLower(name) != name || strings.Contains(name, " ") {
				t.Errorf("%s.%s json tag %q is not snake_case", typ.Name(), field.Name, tag)
			}
		}
	}
}

// webhookReq builds a bare webhook request: no auth context (the route is
// public), raw body bytes, and the signature header exactly as a provider
// would send it.
func webhookReq(body []byte, header string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/payment", strings.NewReader(string(body)))
	if header != "" {
		r.Header.Set(webhookSignatureHeader, header)
	}
	return r
}

// mustStaleWebhook signs a valid-shaped event with a timestamp outside the
// replay tolerance — the "captured yesterday's webhook" forgery the
// verifier must reject on time alone.
func mustStaleWebhook(t *testing.T, gw *paymentadapter.FakeGateway, intent string, amount int) *http.Request {
	t.Helper()
	body, header := gw.SignWebhook(apppayment.WebhookEvent{
		Kind: apppayment.EventCaptured, IntentID: intent, AmountCents: amount,
	}, time.Now().Add(-time.Hour))
	return webhookReq(body, header)
}
