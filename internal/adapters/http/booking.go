package http

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	booking "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
	domainpayment "github.com/nathanfabio/bookingConcurrent/internal/domain/payment"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/middleware"
)

// Booking DTOs. snake_case JSON is a hard rule (CLAUDE.md §4) — the tag
// test in booking_test.go fails the build on drift.
type (
	holdRequest struct {
		ScreeningID string `json:"screening_id"`
		Row         string `json:"row"`
		Number      int    `json:"number"`
	}

	// holdResponse answers POST /holds. HoldToken is NOT here on purpose:
	// it is the server-internal compare-and-delete secret (ADR 0002), and
	// leaking it to clients would let a caller forge teardowns. The client
	// works with session_id only.
	holdResponse struct {
		SessionID   string    `json:"session_id"`
		ScreeningID string    `json:"screening_id"`
		Row         string    `json:"row"`
		Number      int       `json:"number"`
		ExpiresAt   time.Time `json:"expires_at"`
	}

	bookingResponse struct {
		ID          string    `json:"id"`
		SessionID   string    `json:"session_id"`
		ScreeningID string    `json:"screening_id"`
		Row         string    `json:"row"`
		Number      int       `json:"number"`
		Status      string    `json:"status"`
		ConfirmedAt time.Time `json:"confirmed_at"`
	}

	seatMapResponse struct {
		ScreeningID string         `json:"screening_id"`
		MovieID     string         `json:"movie_id"`
		StartsAt    time.Time      `json:"starts_at"`
		Rows        []string       `json:"rows"`
		SeatsPerRow int            `json:"seats_per_row"`
		Seats       []seatResponse `json:"seats"`
	}

	seatResponse struct {
		Row    string `json:"row"`
		Number int    `json:"number"`
		Status string `json:"status"`
	}
)

func bookingDTO(b booking.Booking) bookingResponse {
	return bookingResponse{
		ID:          b.ID,
		SessionID:   b.SessionID,
		ScreeningID: b.ScreeningID,
		Row:         b.Seat.Row,
		Number:      b.Seat.Number,
		Status:      string(b.Status),
		ConfirmedAt: b.ConfirmedAt,
	}
}

// HoldHandler implements POST /holds: claim a seat for checkout. The user
// ID comes from the auth context, NEVER from the body (CLAUDE.md §3).
// Must be wired behind middleware.Auth.
func HoldHandler(svc *appbooking.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := middleware.UserIDFromContext(r.Context())
		if !ok {
			// Defense in depth: wiring bug, not a client error (see MeHandler).
			WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "authentication required")
			return
		}
		var req holdRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return // decodeJSON wrote the 400
		}
		if strings.TrimSpace(req.ScreeningID) == "" {
			WriteError(w, http.StatusBadRequest, CodeValidation, "screening_id is required")
			return
		}
		// Seat SHAPE validation lives at the edge: NewSeat normalizes and
		// rejects malformed rows/numbers before anything domain runs. The
		// screening-geometry check (ErrSeatOutOfRange) stays in the service.
		seat, err := booking.NewSeat(req.Row, req.Number)
		if err != nil {
			WriteError(w, http.StatusBadRequest, CodeValidation, bookingValidationMessage(err))
			return
		}

		hold, err := svc.Hold(r.Context(), userID, req.ScreeningID, seat)
		if err != nil {
			writeBookingError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusCreated, holdResponse{
			SessionID:   hold.SessionID,
			ScreeningID: hold.ScreeningID,
			Row:         hold.Seat.Row,
			Number:      hold.Seat.Number,
			ExpiresAt:   hold.ExpiresAt,
		})
	}
}

// ConfirmHandler implements POST /holds/{sessionID}/confirm: turn the
// caller's live hold into the durable booking. 201 the first time; an
// idempotent replay (same session, already confirmed) returns the same
// booking with 200 (ADR 0006). Must be wired behind middleware.Auth.
func ConfirmHandler(svc *appbooking.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := middleware.UserIDFromContext(r.Context())
		if !ok {
			WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "authentication required")
			return
		}
		res, err := svc.Confirm(r.Context(), userID, r.PathValue("sessionID"))
		if err != nil {
			writeBookingError(w, r, err)
			return
		}
		status := http.StatusCreated
		if !res.Created {
			status = http.StatusOK
		}
		WriteJSON(w, status, bookingDTO(res.Booking))
	}
}

// ReleaseHandler implements DELETE /holds/{sessionID}: give up a hold
// without confirming. 204 on success. Unknown, expired, or foreign
// sessions all get the same 404 (CLAUDE.md §3). Must be wired behind
// middleware.Auth.
func ReleaseHandler(svc *appbooking.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := middleware.UserIDFromContext(r.Context())
		if !ok {
			WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "authentication required")
			return
		}
		if err := svc.Release(r.Context(), userID, r.PathValue("sessionID")); err != nil {
			writeBookingError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// SeatMapHandler implements GET /screenings/{screeningID}/seats: the full
// seat grid with per-seat status, computed server-side (CLAUDE.md §4) with
// Postgres-first precedence (ADR 0006). Public — availability is
// browse-before-login data, like the catalog. The route is screening-scoped
// (not /movies/{id}/seats) because seat state only exists per screening.
func SeatMapHandler(svc *appbooking.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		view, err := svc.SeatMap(r.Context(), r.PathValue("screeningID"))
		if err != nil {
			writeBookingError(w, r, err)
			return
		}
		seats := make([]seatResponse, 0, len(view.Seats))
		for _, st := range view.Seats {
			seats = append(seats, seatResponse{
				Row:    st.Seat.Row,
				Number: st.Seat.Number,
				Status: string(st.Status),
			})
		}
		WriteJSON(w, http.StatusOK, seatMapResponse{
			ScreeningID: view.Screening.ID,
			MovieID:     view.Screening.MovieID,
			StartsAt:    view.Screening.StartsAt,
			Rows:        view.Screening.Rows,
			SeatsPerRow: view.Screening.SeatsPerRow,
			Seats:       seats,
		})
	}
}

// writeBookingError is the sentinel→status mapping for booking (and the
// movie-domain sentinels the booking handlers surface), mirroring
// writeAuthError. Infra errors log server-side with the request ID and
// surface a generic 500 — never details.
//
// The three hold-lookup failures — unknown session, someone else's session,
// expired hold — are deliberately BYTE-IDENTICAL 404s: probing session IDs
// must not reveal which exist, and "expired" versus "gone" is sub-second
// timing noise the client should not be able to observe (CLAUDE.md §3,
// ADR 0006).
func writeBookingError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, booking.ErrSeatAlreadyHeld):
		WriteError(w, http.StatusConflict, CodeSeatTaken, "seat is already held")
	case errors.Is(err, booking.ErrSeatAlreadyBooked):
		WriteError(w, http.StatusConflict, CodeSeatBooked, "seat is already booked")
	case errors.Is(err, booking.ErrHoldLimitExceeded):
		WriteError(w, http.StatusConflict, CodeHoldLimitExceeded, "active hold limit reached")
	case errors.Is(err, booking.ErrSeatOutOfRange):
		WriteError(w, http.StatusBadRequest, CodeValidation, bookingValidationMessage(err))
	case errors.Is(err, booking.ErrHoldNotFound),
		errors.Is(err, booking.ErrNotHoldOwner),
		errors.Is(err, booking.ErrHoldExpired):
		WriteError(w, http.StatusNotFound, CodeNotFound, "hold not found")
	case errors.Is(err, domainpayment.ErrPaymentNotCaptured):
		// The M5 confirm gate (ADR 0008): the hold is live and owned, but
		// the money has not moved. Same body as the payment routes' 402 so
		// clients branch on one code wherever it appears.
		WriteError(w, http.StatusPaymentRequired, CodePaymentRequired, "no captured payment for this session")
	case errors.Is(err, domainmovie.ErrScreeningNotFound):
		WriteError(w, http.StatusNotFound, CodeNotFound, "screening not found")
	case errors.Is(err, domainmovie.ErrMovieNotFound):
		WriteError(w, http.StatusNotFound, CodeNotFound, "movie not found")
	default:
		attrs := []any{slog.String("error", err.Error())}
		if id := middleware.RequestIDFromContext(r.Context()); id != "" {
			attrs = append(attrs, slog.String("request_id", id))
		}
		slog.ErrorContext(r.Context(), "booking request failed", attrs...)
		WriteError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
	}
}

// bookingValidationMessage strips the domain package scaffolding so clients
// get the human-readable half only:
// "booking: seat is outside the screening geometry" -> "seat is outside the
// screening geometry". Same idea as auth's validationMessage.
func bookingValidationMessage(err error) string {
	return strings.TrimPrefix(err.Error(), "booking: ")
}
