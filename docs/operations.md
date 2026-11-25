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
