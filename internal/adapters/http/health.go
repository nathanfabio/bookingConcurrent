package http

import (
	"context"
	"net/http"
	"time"
)

// healthCheckTimeout bounds each readiness probe so one wedged dependency
// cannot stall the whole endpoint past what a load balancer will tolerate.
const healthCheckTimeout = 2 * time.Second

// ReadinessCheck is one named dependency probe for /readyz. M2 wires the
// real Redis and Postgres checks in; until then the endpoint reports ready
// with an empty checks map (the server skeleton itself is live).
type ReadinessCheck struct {
	Name  string
	Check func(ctx context.Context) error
}

// healthResponse is the liveness payload. Liveness answers "is the process
// up at all" and must never depend on downstream services — restarting this
// process cannot fix Redis being down, so failing /healthz on dependency
// trouble would just cause restart storms.
type healthResponse struct {
	Status string `json:"status"`
}

// readyResponse reports per-dependency state. A failing check yields 503
// with the failing check named, so operators see WHICH dependency, not
// just that something is wrong.
type readyResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// HealthzHandler implements GET /healthz (liveness, CLAUDE.md §5).
func HealthzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, healthResponse{Status: "ok"})
	}
}

// ReadyzHandler implements GET /readyz (readiness, CLAUDE.md §5).
// Checks run sequentially with a per-check timeout; the first failure
// still runs the rest so the response shows the full picture.
func ReadyzHandler(checks []ReadinessCheck) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := readyResponse{Status: "ready", Checks: make(map[string]string, len(checks))}
		status := http.StatusOK

		for _, c := range checks {
			ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
			err := c.Check(ctx)
			cancel()

			if err != nil {
				resp.Checks[c.Name] = err.Error()
				resp.Status = "unavailable"
				status = http.StatusServiceUnavailable
			} else {
				resp.Checks[c.Name] = "ok"
			}
		}

		WriteJSON(w, status, resp)
	}
}
