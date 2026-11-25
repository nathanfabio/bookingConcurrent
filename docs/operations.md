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

An authorized request stays pending. The next slice will execute it through
the sandbox gateway and record completion. For recovery, retain both the
payment and its request. Rolling back migration 00012 drops the request
ledger and removes the booking guard; preserve the ledger and resolve any
pending requests before considering a schema downgrade.
