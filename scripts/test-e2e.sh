#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
work=$(mktemp -d)
suffix=${work##*.}
compose=(docker compose -p "booking-e2e-${suffix,,}" -f test/e2e/compose.yaml)
mode=${1:-journey}
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
export E2E_CHECKOUT="$work/checkout.json"
# The outage probes log both test users back in across process boundaries.
export RATE_LIMIT_AUTH_BURST=8
if [[ "$mode" == contention ]]; then
  # The load driver shares one loopback IP across all authenticated buyers.
  export RATE_LIMIT_GENERAL_BURST=512 RATE_LIMIT_AUTH_BURST=32 RATE_LIMIT_HOLD_BURST=256
fi
export E2E_WORKER_LOG="$work/worker.log"
"$work/api" >"$work/api.log" 2>&1 &
api_pid=$!
for ((i=0; i<100; i++)); do
  kill -0 "$api_pid"
  if curl -fsS --max-time 1 "http://$HTTP_ADDR/readyz" >/dev/null 2>&1; then break; fi
  sleep 0.2
done
curl -fsS --max-time 2 "http://$HTTP_ADDR/readyz" >/dev/null
if [[ "$mode" == contention ]]; then
  "$work/journey" -test.run='^TestHTTPHoldContention$' -test.v -test.timeout=180s
  exit 0
fi
psql=( "${compose[@]}" exec -T postgres psql -XAtq -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" )

start_worker() {
  LOG_LEVEL=debug "$work/worker" "$@" >>"$E2E_WORKER_LOG" 2>&1 &
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
"$work/journey" -test.run='^TestBookingJourney$' -test.timeout=120s >"$work/journey.log" 2>&1 &
journey_pid=$!
wait_for "failed publication while broker is offline" grep -q 'outbox relay pass failed' "$E2E_WORKER_LOG"

wait_for "booked seat visible through HTTP during outage" test -s "$E2E_CHECKOUT"

# This project has one checkout. Capture its event identity and reuse it
# for every subsequent assertion, including the controlled duplicate.
booking_id=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["Booking"]["ID"])' <"$E2E_CHECKOUT")
pending_event() {
  "${psql[@]}" -v booking_id="$booking_id" <<'SQL'
  SELECT o.event_id FROM outbox o
  JOIN confirmed_bookings b ON b.id::text=o.payload->>'booking_id'
  WHERE b.id=:'booking_id' AND b.status='confirmed' AND o.event_type='BookingConfirmed'
    AND o.published_at IS NULL
    AND NOT EXISTS (SELECT 1 FROM processed_events p WHERE p.event_id=o.event_id);
SQL
}
event_id=$(pending_event)
[[ "$event_id" =~ ^[0-9a-f-]{36}$ ]] || { echo "expected one pending, unprocessed booking event" >&2; exit 1; }
# Snapshot the actual pending transaction before reconnecting the source broker.
pg=( "${compose[@]}" exec -T postgres )
"${pg[@]}" pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc >"$work/pending.dump"
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

# Move Redis off its published TCP port and disconnect established clients.
# The alternate listener is reachable only inside this disposable project.
redisctl=( "${compose[@]}" exec -T redis redis-cli -e )
"${redisctl[@]}" CONFIG SET port 6380
"${redisctl[@]}" -p 6380 CLIENT KILL TYPE normal SKIPME yes
"$work/journey" -test.run='^TestRedisUnavailable$' -test.timeout=60s
"${redisctl[@]}" -p 6380 CONFIG SET port 6379
wait_for "API ready after Redis reconnects" curl -fsS --max-time 3 "http://$HTTP_ADDR/readyz"
"$work/journey" -test.run='^TestRedisRecovered$' -test.timeout=60s
wait_for "original sale and event after Redis recovery" assert_delivery

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
  if [[ "$done_count" == 1 ]]; then break; fi
  sleep 0.2
done
[[ "$done_count" == 1 ]] || { echo "worker did not complete the authorized sandbox refund" >&2; exit 1; }

refund_snapshot() {
  "${psql[@]}" -c "
    SELECT p.id, p.intent_id, s.reference, r.amount_cents, r.currency, r.completed_at
    FROM payments p JOIN refund_requests r ON r.payment_id=p.id
    JOIN sandbox_refund_receipts s ON s.idempotency_key=p.id AND s.reference=r.provider_reference
    WHERE p.intent_id='pi_fake_refund_e2e' AND p.status='refunded' AND r.completed_at IS NOT NULL"
}
original_refund=$(refund_snapshot)
[[ -n "$original_refund" ]]
"${pg[@]}" pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc >"$work/booking.dump"

stop_apps() {
  kill -TERM "$api_pid" "$worker_pid"
  wait "$api_pid"
  api_pid=
  wait "$worker_pid"
  worker_pid=
}

restore_target() {
  local archive=$1
  export POSTGRES_DB=$2 REDIS_DB=$3
  "${pg[@]}" createdb -U "$POSTGRES_USER" -T template0 "$POSTGRES_DB"
  "${pg[@]}" pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --single-transaction <"$archive"
  psql=( "${pg[@]}" psql -XAtq -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" )
  [[ $("${redisctl[@]}" -n "$REDIS_DB" DBSIZE) == 0 ]]
  "${rabbitctl[@]}" add_vhost "$POSTGRES_DB"
  "${rabbitctl[@]}" set_permissions -p "$POSTGRES_DB" booking '.*' '.*' '.*'
  export RABBITMQ_URL="${RABBITMQ_URL%/*}/$POSTGRES_DB"
}

stop_apps
restore_target "$work/booking.dump" booking_restored 1
export ENV=production
if [[ "$mode" == contention ]]; then
  # The load driver shares one loopback IP across all authenticated buyers.
  export RATE_LIMIT_GENERAL_BURST=512 RATE_LIMIT_AUTH_BURST=32 RATE_LIMIT_HOLD_BURST=256
fi
export E2E_WORKER_LOG="$work/restored-worker.log"
"$work/api" >"$work/restored-api.log" 2>&1 &
api_pid=$!
start_worker
wait_for "restored API readiness" curl -fsS --max-time 3 "http://$HTTP_ADDR/readyz"
wait_for "restored worker subscription" grep -q 'consumer subscribed' "$E2E_WORKER_LOG"
"$work/journey" -test.run='^TestRestoredBooking$' -test.timeout=30s
assert_delivery
original_payment_id=${original_refund%%|*}
decision=$("${psql[@]}" -v payment_id="$original_payment_id" <<'SQL'
SELECT request_sandbox_refund(:'payment_id');
SQL
)
[[ "$decision" == already_requested && "$(refund_snapshot)" == "$original_refund" ]]
[[ $("${psql[@]}" -c 'SELECT count(*) FROM sandbox_refund_receipts') == 1 ]]
echo "Postgres restore verified: original booking, event and sandbox refund receipt"

# Rewind the same sale to a point before publication. The source was able
# to deliver later; isolate this timeline from every previous broker vhost.
stop_apps
restore_target "$work/pending.dump" booking_pending_restore 2
[[ "$(pending_event)" == "$event_id" ]] || { echo "archive missing original pending event" >&2; exit 1; }
if [[ "$mode" == contention ]]; then
  # The load driver shares one loopback IP across all authenticated buyers.
  export RATE_LIMIT_GENERAL_BURST=512 RATE_LIMIT_AUTH_BURST=32 RATE_LIMIT_HOLD_BURST=256
fi
export E2E_WORKER_LOG="$work/pending-restore-worker.log"
start_worker
wait_for "restored pending event published and claimed" assert_delivery
notified_restored_booking() {
  grep -F 'booking confirmation notification (stub)' "$E2E_WORKER_LOG" | grep -F "$booking_id" >/dev/null
}
wait_for "restored worker notification for original booking" notified_restored_booking
echo "Pending outbox restore verified: booking $booking_id, event $event_id"

# Recover another isolated copy of the real sale with only its consumer
# paused. The publisher must actually route the message before we lose it.
kill -TERM "$worker_pid"
wait "$worker_pid"
worker_pid=
restore_target "$work/pending.dump" booking_queue_loss 3
if [[ "$mode" == contention ]]; then
  # The load driver shares one loopback IP across all authenticated buyers.
  export RATE_LIMIT_GENERAL_BURST=512 RATE_LIMIT_AUTH_BURST=32 RATE_LIMIT_HOLD_BURST=256
fi
export E2E_WORKER_LOG="$work/queue-loss-worker.log"
start_worker --without-consumer
publication_stamp() {
  "${psql[@]}" -v id="$event_id" <<'SQL'
SELECT published_at FROM outbox WHERE event_id=:'id';
SQL
}
queue_depth() {
  local counts
  counts=$("${rabbitctl[@]}" list_queues -p "$POSTGRES_DB" --quiet --no-table-headers name messages_ready consumers)
  [[ "$counts" == "$RABBITMQ_QUEUE"$'\t'"$1"$'\t0' ]]
}
wait_for "one queued message and zero consumers" queue_depth 1
publication_recorded() {
  first_stamp=$(publication_stamp) && [[ -n "$first_stamp" ]]
}
wait_for "first publication recorded" publication_recorded
"${rabbitctl[@]}" purge_queue -p "$POSTGRES_DB" "$RABBITMQ_QUEUE"
wait_for "queue message lost" queue_depth 0
[[ $("${psql[@]}" -c 'SELECT count(*) FROM processed_events') == 0 ]]
[[ $("$work/worker" republish "$event_id") == republished ]]
wait_for "original event queued again" queue_depth 1
kill -TERM "$worker_pid"
wait "$worker_pid"
worker_pid=
start_worker
wait_for "manual republication processed" assert_delivery
wait_for "manual republication notified original booking" notified_restored_booking
[[ $("$work/worker" republish "$event_id") == already_processed ]]
[[ "$(publication_stamp)" == "$first_stamp" ]]
echo "Manual recovery verified for published event $event_id after queue loss"
