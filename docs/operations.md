# Running the packaged booking stack locally

The optional Compose overlay runs API, worker and schema migration images
against the services and volume in the base Compose project. Existing
development records stay in that volume. A fresh packaged database starts
with an empty catalog because the development seed only runs in development.
For a disposable checkout with sample screenings, use `make test-e2e`.

## Start and check the stack

Use Docker Compose v2. On first setup copy `.env.example` to `.env`. Set
`AUTH_JWT_SECRET` and `PAYMENT_FAKE_WEBHOOK_SECRET` to separate values
generated with `openssl rand -hex 32`. Keep the Postgres user, password and
database aligned with the local Compose service. The overlay sets the
container DNS names, production application mode and the fake payment
provider. This is a local deployment exercise with development database
and broker credentials. Production cookie settings require HTTPS for a
browser refresh-token flow.

Run the following from the repository root in Bash:

```bash
stack=(docker compose -f compose.yaml -f compose.apps.yaml)
"${stack[@]}" up -d --build
"${stack[@]}" ps -a
curl --fail http://localhost:8080/readyz
curl --fail http://localhost:8080/movies
"${stack[@]}" logs --tail 30 worker migrate
```

Stop any host API and worker using this infrastructure before switching
modes. Set `API_HOST_PORT` to change the published loopback port and adjust
the curl commands accordingly. API readiness checks Postgres and Redis.
Inspect worker logs for broker connectivity and notification delivery.
A successful migration container displays `Exited (0)`.

## Schema changes and recovery

The migration job runs the SQL embedded in its image. Both application
services wait for its successful completion. For a subsequent release, build
the images, review migration status and apply pending migrations explicitly:

```bash
"${stack[@]}" build
"${stack[@]}" run --rm migrate status
"${stack[@]}" run --rm migrate up
"${stack[@]}" up -d api worker
```

If migration fails, inspect the migrate logs and Postgres health. Correct
the reported configuration or migration issue, rebuild if code changed,
then repeat the status and up steps before starting the new binaries. A
migration failure blocks fresh dependent containers. Already-running older
instances require schema-compatible migrations during an upgrade.

For HTTP 503 on readiness, inspect Redis and Postgres first. For delayed
notifications with a healthy API, inspect RabbitMQ and worker logs. The
committed outbox rows let the relay resume after the broker recovers.

## Stop and restart

```bash
"${stack[@]}" stop api worker
"${stack[@]}" ps -a
"${stack[@]}" down
```

Compose sends SIGTERM directly to the Go processes. The API drains requests
and the worker cancels its jobs. Check for exit code zero. The 30-second
grace period accommodates the default HTTP shutdown and telemetry flush
timeouts. Increase it when raising the HTTP drain timeout. Ordinary
`down` retains the named Postgres volume. Repeat the startup commands to
restart.

## Review captured payments without bookings

Apply migration 00011, then query the operational view in the local Compose
Postgres. It includes the currency and frozen amount for reconciliation.

```bash
docker compose exec -T postgres sh -c 'psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <<'SQL'
SELECT payment_id, booking_session_id, intent_id, amount_cents, currency,
       captured_at, hold_expires_at, review_state
FROM unbooked_captures
ORDER BY captured_at, payment_id;
SQL
```

| Review state | Operational meaning |
|---|---|
| `before_deadline` | Checkout may still complete. Recheck after the recorded deadline. |
| `deadline_passed` | Check for a committed booking again and investigate the capture using the session and intent IDs. |
| `unknown_deadline` | A historical writer omitted the deadline. Review individually. |

The report uses database time. A capture arriving after the deadline appears
as soon as its transaction commits. Reading the report leaves payment state unchanged.

### Authorize a reviewed sandbox refund

After applying migration 00012, the operations owner can authorize a full
refund by payment ID. Use the database owner in local Compose. For a shared
deployment, use an individually attributable operator role and grant the
required table privileges and function execution explicitly. The function
runs with the caller's privileges. A shared database login records that
login as the reviewer.

Open psql and enter the reviewed UUID when prompted:

```bash
docker compose exec postgres sh -c 'psql -X -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
```

```sql
\prompt 'Reviewed payment UUID: ' payment_id
SELECT request_sandbox_refund(:'payment_id'::uuid);
SELECT * FROM refund_requests WHERE payment_id = :'payment_id'::uuid;
```

Use autocommit at the default READ COMMITTED isolation. A result of
`requested` records the amount, currency, database role and time and moves
the payment to `refunding`. If the connection drops, repeat the same call.
`already_requested` identifies the existing request. `booking_exists`
preserves the sale. The other outcomes identify an unknown payment,
unsupported gateway, missing capture, missing deadline, or future deadline.
Historical captures with unknown deadlines still require manual investigation.

After migration 00013, the worker polls for authorized sandbox refunds
every `WORKER_REFUND_POLL_INTERVAL` (default 5s), up to
`WORKER_REFUND_BATCH_SIZE` (default 50) per pass. Repeat the request query
above to see the last attempt, completion time and provider receipt. The
worker sets the payment to `refunded` in the transaction that records
completion. Repeating the authorization still returns `already_requested`.

If a request remains pending, check worker logs for its payment ID and
verify Postgres connectivity. The worker retries the original payment ID
after restart, including when the provider accepted a refund and its
response was lost. A mismatch in amount, currency, intent or receipt
requires investigation. Keep the original authorization and receipt intact.
Rolling back migrations 00012 or 00013 drops audit or idempotency records.
Preserve those records and reconcile outstanding operations before a downgrade.

## Diagnose a delayed booking notification

Run `make test-e2e` to reproduce the outage locally. The runner stops the
RabbitMQ application inside a fresh Compose project while the API and
worker are running. It checks out a seat, restarts the worker while the
broker is still down and restores the broker after the new relay attempts
publication. The client reads the booked seat during the outage and repeats
confirmation after the notification arrives.

To trace a delayed notification in an existing installation, use the booking
ID returned by the API. Run this read-only query in psql:

```sql
\prompt 'Booking UUID: ' booking_id
SELECT b.id, b.status, o.event_id, o.created_at, o.published_at, p.processed_at
FROM confirmed_bookings b
LEFT JOIN outbox o ON o.payload->>'booking_id' = b.id::text
                  AND o.event_type = 'BookingConfirmed'
LEFT JOIN processed_events p ON p.event_id = o.event_id
WHERE b.id = :'booking_id'::uuid;
```

A booking with a pending outbox row has committed. Check the worker relay
logs and RabbitMQ connectivity. A null publication timestamp means the
relay has yet to record broker acceptance; the broker may already have
received the message. If publication is recorded and the claim is absent,
inspect the queue, its consumers and worker reconnection logs. A claim
records the consumer's deduplication decision. For the current stub, look
for the notification log with the same booking ID to verify the side effect.

The drill also clears the publication timestamp for its own disposable event
to reproduce a relay retry after broker acceptance. It observes two deliveries
of the same event ID, one consumer claim and one notification. During incident
recovery, restore connectivity and let the relay process pending rows.
[ADR 0009](adr/0009-messaging-rabbitmq-outbox-relay.md) describes the
claim-before-notify crash window.

## Diagnose a Redis connectivity failure

Check the API directly with `GET /healthz` and `GET /readyz`. During the
Redis outage drill, liveness returns 200 and readiness returns 503, with
Postgres reporting `ok` and Redis reporting a connection error. A load
balancer using readiness can drain the instance, so its external behavior
can differ from the direct API observations below.

| Direct API request | Expected result while Redis is unreachable |
|---|---|
| Owner repeats confirmation for a committed session | 200 with the original booking from Postgres |
| A different user repeats that confirmation | 404 with `not_found` |
| Owner requests a fresh hold for an available seat | 500 with the current `internal_error` envelope |

Restore Redis connectivity and check readiness again. The API should
return 200 without a process restart. Read the screening's seat map to
verify the committed seat remains booked. For the connection failure
exercised here, the attempted new hold never reached Redis, so its seat
remains available and can be held and released. A timeout *after* a write
reaches Redis is a different failure mode.

To reproduce, run `make test-e2e`. Within its disposable Compose project,
the runner moves the Redis TCP listener off the mapped port and closes
existing client connections, then restores the listener. Redis data
survives the drill. The final database check verifies that confirmation
replay produced no extra booking or outbox event.
