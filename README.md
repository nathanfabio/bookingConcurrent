# bookingConcurrent
A software of simultaneous reservations

This software is designed to handle multiple reservations at the same time, ensuring that users can book their desired slots without conflicts. It is ideal for businesses that require efficient management of appointments, events, or services.

## It was created for a study and I'm still working on it.

Two binaries, one hexagonal codebase (`internal/domain` → `application` →
`ports` → `adapters`; see `docs/adr/`):

- `cmd/api` — HTTP service: auth, catalog, seat map, holds, payments.
- `cmd/worker` — background jobs: the transactional-outbox relay, the
  `BookingConfirmed` consumer (ADR 0009), and the hold-expiry sweeper
  (ADR 0010).

### Running locally

```bash
cp .env.example .env      # then edit (secrets are generated, never committed)
make up                   # Redis + Postgres + RabbitMQ + UIs via docker compose
make migrate              # explicit migration (dev also auto-migrates at boot)
make run                  # the API on :8080
make run-worker           # the background worker (separate terminal)
make test-race            # unit + integration tests, -race (infra optional:
                          # integration tests skip when it is unreachable)
```

### Event pipeline (M6, ADR 0009)

A confirmed booking and its event commit in ONE Postgres transaction; the
worker's relay publishes it to RabbitMQ only after the broker confirms, and
the consumer dedupes on the event id — at-least-once delivery, exactly-once
effect:

```
POST /holds/{id}/confirm
   └─ tx: confirmed_bookings + outbox row ──┐ (ADR 0006 commit point)
                                            ▼
                       cmd/worker: relay (poll, publish, confirm, mark)
                                            ▼
                    exchange booking.events ──► queue booking.notifications
                                            ▼
                       consumer: processed_events claim ──► notify (log stub)
```

Environment reference: `.env.example` (every variable is validated at boot —
the process refuses to start half-configured). API contract: `docs/api.md`.
Decisions and their reasoning: `docs/adr/`.