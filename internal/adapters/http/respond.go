// Package http contains the HTTP adapters: handlers, DTOs, and the shared
// response envelope (CLAUDE.md §1, §5).
//
// Note on naming: this package is `package http` living under adapters, so
// files INSIDE it import the standard library's net/http without ceremony;
// files elsewhere import this package with an alias (e.g. httpadapter).
package http

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Error codes form the stable, machine-readable half of the error contract.
// Messages are for humans and may change; codes are what clients branch on.
const (
	CodeInternal     = "internal_error"
	CodeNotFound     = "not_found"
	CodeUnauthorized = "unauthorized"     // M3: auth required / credentials rejected
	CodeValidation   = "validation_error" // M3: request body failed validation
	CodeConflict     = "conflict"         // M3: duplicate email on register

	// M4 booking codes. They are specific (not the generic CodeConflict)
	// because clients branch on them: seat_taken may free up when a hold
	// expires, seat_booked never does, and hold_limit_exceeded tells the
	// buyer to finish or release something first.
	CodeSeatTaken         = "seat_taken"          // a live hold owns the seat
	CodeSeatBooked        = "seat_booked"         // a confirmed booking owns the seat
	CodeHoldLimitExceeded = "hold_limit_exceeded" // per-user active hold cap
)

// errorResponse is the single JSON shape every error path writes
// (CLAUDE.md §5). Keeping it private forces all error writes through
// WriteError, which is what makes the "no silent 200 on failure" rule
// structural instead of aspirational.
type errorResponse struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

// WriteJSON writes v as the response body with the given status.
// It sets Content-Type before WriteHeader, as required for it to land.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Headers are already on the wire; the only honest move is to log.
		// The client sees a truncated body rather than a misleading one.
		slog.Error("failed to encode response body", slog.Any("error", err))
	}
}

// WriteError writes the standard error envelope. Every handler error path
// must end here (or in a wrapper that ends here) — logging an error and
// returning without writing a response is the exact bug class this
// project's reference implementation had (silent 200 OK on failure).
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, errorResponse{Message: message, Code: code})
}

// WriteInternalError is the panic-recovery writer injected into
// middleware.Recovery by the composition root. It intentionally reveals
// nothing about the panic to the client — the details are already in the
// server-side log with the request ID for correlation.
func WriteInternalError(w http.ResponseWriter, r *http.Request, recovered any) {
	WriteError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
}

// WriteUnauthorized is the 401 writer injected into middleware.Auth by the
// composition root. Like WriteInternalError it keeps the message generic:
// distinguishing "no token" from "bad signature" from "expired" in the
// response would hand attackers a debugging tool for free (ADR 0004).
func WriteUnauthorized(w http.ResponseWriter, r *http.Request) {
	WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "authentication required")
}
