package postgres

import (
	"reflect"
	"strings"
	"testing"
)

// TestBookingOutboxPayloadSnakeCase enforces CLAUDE.md §4 on the event
// contract: every field of the BookingConfirmed outbox payload must carry a
// snake_case json tag. The M6 relay and its consumers read these names, so
// a rename must be deliberate, not accidental drift.
func TestBookingOutboxPayloadSnakeCase(t *testing.T) {
	typ := reflect.TypeFor[bookingConfirmedPayload]()
	for field := range typ.Fields() {
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

// TestBookingOutboxPayloadFieldSetMatchesContract is the WRITER-side twin of
// TestBookingConfirmedFieldSetMatchesWriterContract in
// application/notification: the payload struct is deliberately duplicated on
// the consumer side (consumer-owned contract), and both copies are pinned
// against the same transcription of docs/api.md's field table. Changing one
// side without the other fails a test on BOTH sides.
func TestBookingOutboxPayloadFieldSetMatchesContract(t *testing.T) {
	wantTags := map[string]string{
		"BookingID":   "booking_id",
		"SessionID":   "session_id",
		"ScreeningID": "screening_id",
		"Row":         "row",
		"Number":      "number",
		"UserID":      "user_id",
		"ConfirmedAt": "confirmed_at",
	}
	typ := reflect.TypeFor[bookingConfirmedPayload]()
	if typ.NumField() != len(wantTags) {
		t.Errorf("bookingConfirmedPayload has %d fields, contract has %d — update docs/api.md AND application/notification.BookingConfirmed",
			typ.NumField(), len(wantTags))
	}
	for field := range typ.Fields() {
		want, ok := wantTags[field.Name]
		if !ok {
			t.Errorf("field %s is not in the wire contract", field.Name)
			continue
		}
		if got := strings.Split(field.Tag.Get("json"), ",")[0]; got != want {
			t.Errorf("field %s json tag = %q, contract says %q", field.Name, got, want)
		}
	}
}
