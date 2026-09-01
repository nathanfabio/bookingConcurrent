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
	dtos := []any{bookingConfirmedPayload{}}
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
