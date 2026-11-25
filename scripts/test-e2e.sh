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
