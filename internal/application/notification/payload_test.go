package notification_test

import (
	"reflect"
	"strings"
	"testing"

	appnotification "github.com/nathanfabio/bookingConcurrent/internal/application/notification"
)

// TestBookingConfirmedPayloadSnakeCase is the CONSUMER-side twin of
// TestBookingOutboxPayloadSnakeCase in adapters/postgres: the writer and the
// consumer each pin their copy of the wire contract to snake_case
// (CLAUDE.md §4's hard rule). The structs are deliberately duplicated
// (consumer-owned contract, package doc of application/notification); these
// two reflection tests plus the docs/api.md field table are what keep the
// copies honest.
func TestBookingConfirmedPayloadSnakeCase(t *testing.T) {
	typ := reflect.TypeFor[appnotification.BookingConfirmed]()
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

// TestBookingConfirmedFieldSetMatchesWriterContract pins the FIELD SET
// (names + tags, order-insensitive) so adding/removing a field on either
// side without updating the docs and the twin struct fails a test. The
// writer's struct is unexported in another package, so the contract list is
// transcribed here from docs/api.md — the same source of truth both sides
// cite.
func TestBookingConfirmedFieldSetMatchesWriterContract(t *testing.T) {
	wantTags := map[string]string{
		"BookingID":   "booking_id",
		"SessionID":   "session_id",
		"ScreeningID": "screening_id",
		"Row":         "row",
		"Number":      "number",
		"UserID":      "user_id",
		"ConfirmedAt": "confirmed_at",
	}
	typ := reflect.TypeFor[appnotification.BookingConfirmed]()
	if typ.NumField() != len(wantTags) {
		t.Errorf("BookingConfirmed has %d fields, contract has %d — update docs/api.md AND the writer's bookingConfirmedPayload",
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
