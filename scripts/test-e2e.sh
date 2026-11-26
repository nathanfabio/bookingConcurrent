#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
work=$(mktemp -d)
suffix=${work##*.}
compose=(docker compose -p "booking-e2e-${suffix,,}" -f test/e2e/compose.yaml)
api_pid=
worker_pid=
cleanup() {
  status=$?
  for pid in "$worker_pid" "$api_pid"; do
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
export E2E_WORKER_LOG="$work/worker.log"
"$work/api" >"$work/api.log" 2>&1 &
api_pid=$!
for ((i=0; i<100; i++)); do
  kill -0 "$api_pid"
  if curl -fsS --max-time 1 "http://$HTTP_ADDR/readyz" >/dev/null 2>&1; then break; fi
  sleep 0.2
done
curl -fsS --max-time 2 "http://$HTTP_ADDR/readyz" >/dev/null
"$work/worker" >"$E2E_WORKER_LOG" 2>&1 &
worker_pid=$!
go test -tags=e2e -race -count=1 -timeout=60s ./test/e2e

# The operator's entry point is SQL. Seed a reviewed expired capture in this
# disposable database, then observe the production worker finish the request.
psql=( "${compose[@]}" exec -T postgres psql -XAtq -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" )
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
