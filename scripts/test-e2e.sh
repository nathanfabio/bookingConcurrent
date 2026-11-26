#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
work=$(mktemp -d)
suffix=${work##*.}
compose=(docker compose -p "booking-e2e-${suffix,,}" -f test/e2e/compose.yaml)
api_pid=
worker_pid=
journey_pid=
cleanup() {
  status=$?
  for pid in "$journey_pid" "$worker_pid" "$api_pid"; do
    if [[ -n "$pid" ]]; then
      kill -TERM "$pid" 2>/dev/null || true
      wait "$pid" || status=$?
    fi
  done
  if (( status != 0 )); then
    tail -n 40 "$work"/*.log 2>/dev/null || true
  fi
  "${compose[@]}" down --volumes
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
go build -race -o "$work/" ./cmd/api ./cmd/worker
go test -tags=e2e -race -c -o "$work/journey" ./test/e2e
"${compose[@]}" up -d --wait --wait-timeout 120

set -a
source .env.example
set +a
export OTEL_ENABLED=false
export AUTH_JWT_SECRET=$(openssl rand -hex 32)
export PAYMENT_FAKE_WEBHOOK_SECRET=$(openssl rand -hex 32)
addr=$("${compose[@]}" port postgres 5432)
export POSTGRES_PORT=${addr##*:}
export REDIS_ADDR=$("${compose[@]}" port redis 6379)
addr=$("${compose[@]}" port rabbitmq 5672)
export RABBITMQ_URL="amqp://booking:booking-dev-password@$addr/booking"
export HTTP_ADDR="127.0.0.1:$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
export WORKER_RELAY_POLL_INTERVAL=250ms
export WORKER_REFUND_POLL_INTERVAL=250ms
export E2E_CONFIRMED_ID="$work/confirmed-id"
export E2E_WORKER_LOG="$work/worker.log"
"$work/api" >"$work/api.log" 2>&1 &
api_pid=$!
for ((i=0; i<100; i++)); do
  kill -0 "$api_pid"
  if curl -fsS --max-time 1 "http://$HTTP_ADDR/readyz" >/dev/null 2>&1; then break; fi
  sleep 0.2
done
curl -fsS --max-time 2 "http://$HTTP_ADDR/readyz" >/dev/null
psql=( "${compose[@]}" exec -T postgres psql -XAtq -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" )

start_worker() {
  LOG_LEVEL=debug "$work/worker" >>"$E2E_WORKER_LOG" 2>&1 &
  worker_pid=$!
}
wait_for() {
  local description=$1
  shift
  local deadline=$((SECONDS + 40))
  until "$@"; do
    kill -0 "$worker_pid"
    if (( SECONDS >= deadline )); then
      echo "timed out waiting for $description" >&2
      return 1
    fi
    sleep 0.2
  done
}
start_worker
wait_for "initial subscription" grep -q 'consumer subscribed' "$E2E_WORKER_LOG"

# Stop the Rabbit application inside the disposable container. Its mapped
# port stays fixed so the restarted worker uses precisely the same URL.
rabbitctl=( "${compose[@]}" exec -T rabbitmq rabbitmqctl --timeout 30 )
"${rabbitctl[@]}" stop_app
"$work/journey" -test.timeout=120s >"$work/journey.log" 2>&1 &
journey_pid=$!
wait_for "failed publication while broker is offline" grep -q 'outbox relay pass failed' "$E2E_WORKER_LOG"

wait_for "booked seat visible through HTTP during outage" test -s "$E2E_CONFIRMED_ID"

# This project has one checkout. Capture its event identity and reuse it
# for every subsequent assertion, including the controlled duplicate.
event_id=$("${psql[@]}" -v booking_id="$(cat "$E2E_CONFIRMED_ID")" <<'SQL'
  SELECT o.event_id FROM outbox o
  JOIN confirmed_bookings b ON b.id::text=o.payload->>'booking_id'
  WHERE b.id=:'booking_id' AND b.status='confirmed' AND o.event_type='BookingConfirmed'
    AND o.published_at IS NULL
    AND NOT EXISTS (SELECT 1 FROM processed_events p WHERE p.event_id=o.event_id);
SQL
)
[[ "$event_id" =~ ^[0-9a-f-]{36}$ ]] || { echo "expected one pending, unprocessed booking event" >&2; exit 1; }
kill -TERM "$worker_pid"
wait "$worker_pid"
worker_pid=
cp "$E2E_WORKER_LOG" "$work/worker-before-restart.log"
: >"$E2E_WORKER_LOG"
start_worker
wait_for "restarted relay attempting pending event" grep -q 'outbox relay pass failed' "$E2E_WORKER_LOG"
"${rabbitctl[@]}" start_app
wait "$journey_pid"
journey_pid=

# The HTTP client replayed the confirmation after observing notification.
assert_delivery() {
  local counts
  counts=$("${psql[@]}" -v event_id="$event_id" <<'SQL'
SELECT (SELECT count(*) FROM confirmed_bookings),
       (SELECT count(*) FROM outbox),
       (SELECT count(*) FROM outbox WHERE event_id=:'event_id' AND published_at IS NOT NULL),
       (SELECT count(*) FROM processed_events WHERE event_id=:'event_id' AND event_type='BookingConfirmed');
SQL
  ) || return 1
  [[ "$counts" == '1|1|1|1' ]]
}
wait_for "one sale, one published event and one consumer claim" assert_delivery

# Recreate the durable state after broker acceptance but before the relay
# records its stamp. Only this disposable fixture is rewound.
"${psql[@]}" -v event_id="$event_id" <<'SQL'
UPDATE outbox SET published_at=NULL WHERE event_id=:'event_id';
SQL
duplicate_seen() {
  grep -F 'duplicate delivery skipped' "$E2E_WORKER_LOG" | grep -F "event_id=$event_id" >/dev/null
}
wait_for "duplicate delivery of the original event" duplicate_seen
wait_for "republished event and original claim" assert_delivery
[[ $(grep -c 'booking confirmation notification (stub)' "$E2E_WORKER_LOG") == 1 ]] || {
  echo "expected one notification across both deliveries" >&2
  exit 1
}
echo "RabbitMQ outage and recovery verified for event $event_id"

# The operator's entry point is SQL. Seed a reviewed expired capture in this
# disposable database, then observe the production worker finish the request.
decision=$("${psql[@]}" <<'SQL'
INSERT INTO payments (booking_session_id,gateway,intent_id,status,amount_cents,currency,hold_expires_at)
VALUES ('worker-refund-e2e','fake','pi_fake_refund_e2e','captured',1450,'usd',now()-interval '1 minute')
RETURNING id AS payment_id \gset
SELECT request_sandbox_refund(:'payment_id');
SQL
)
[[ "$decision" == requested ]]
for ((i=0; i<50; i++)); do
  kill -0 "$worker_pid"
  done_count=$("${psql[@]}" -c "
    SELECT count(*) FROM payments p
    JOIN refund_requests r ON r.payment_id=p.id
    JOIN sandbox_refund_receipts s ON s.idempotency_key=p.id AND s.reference=r.provider_reference
    WHERE p.intent_id='pi_fake_refund_e2e' AND p.status='refunded' AND r.completed_at IS NOT NULL")
  if [[ "$done_count" == 1 ]]; then exit 0; fi
  sleep 0.2
done
echo "worker did not complete the authorized sandbox refund" >&2
exit 1
