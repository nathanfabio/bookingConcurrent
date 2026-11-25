# ADR 0012: Observability — bounded traces, correlated logs, truthful metrics

Status: Accepted

## Context

Individual Redis, Postgres and RabbitMQ operations are difficult to explain
without their request/job context. Request ID alone does not form a span tree.
Likewise, counting HTTP successes is not the same as counting new sales: a
client can replay confirmation, and webhook delivery is at-least-once. Redis
expiry is silent, so increment/decrement accounting cannot maintain a correct
active-holds gauge.

Instrumentation must not introduce infrastructure into the domain or use
cases, expose credentials, create unbounded metric cardinality, or make the
booking service depend on an observability backend being online.

## Decision

### Traces and lifecycle

Keep the existing OpenTelemetry SDK and W3C TraceContext propagation. The
API starts a server span inside logging and outside CORS/rate limiting/auth.
Name spans using method + registered ServeMux route template after routing;
requests rejected before routing and unknown routes use `unmatched`. Record
HTTP status and request ID. Mark server failures/panics as span errors; 4xx
client responses retain unset status. A panic is rethrown to outer recovery,
which remains responsible for the HTTP response. Do not attach request
bodies, query strings, cookies, authorization or webhook signature headers.

Redis hooks create child client spans for operations/pipelines under a
recording parent. They export operation names only, never arguments, keys,
Lua source, or raw errors. Expected missing keys are not trace failures.
Postgres uses otelpgx with SQL text, query parameters and connection-detail
attributes disabled, and generic query span names. Background relay/sweep
ticks start their own root spans. Existing RabbitMQ publisher/consumer spans
propagate context through AMQP headers, so worker SQL/Redis calls and
notifications share their job/delivery trace.

Export via the official OTLP/HTTP exporter to the configured BASE URL plus
`/v1/traces`. An explicit endpoint is required when OTLP is enabled (no
hardcoded deployment address); `.env.example` targets Jaeger in Compose
on port 4318. Disabled tracing/stdout need no network endpoint. CI disables
external tracing and tests export against an in-process receiver explicitly.
`OTEL_TRACES_EXPORTER=stdout` retains newline-delimited JSON spans for study
and offline debugging. The Compose Jaeger image is pinned and runs the
dev all-in-one configuration with in-memory storage; it is not production
trace storage. See the [OTel Go exporters](https://opentelemetry.io/docs/languages/go/exporters/)
and [Jaeger deployment example](https://www.jaegertracing.io/docs/2.23/getting-started/).

The batch queue is bounded (2048 spans), exports have a configurable timeout
(default 3s), and each batch gets one attempt. Collector outages lose traces,
not sales; neither startup nor readiness waits for Jaeger. Trace export is
best-effort, unlike the transactional outbox. SDK export failures are logged.
Register SDK shutdown immediately after setup in both binaries so startup
failures also clean up; drain with an uncancelled context capped at 5s.

Use parent-based sampling, with configurable root ratio in [0,1] (default 1
for learning). A valid upstream sampling decision is honored. Invalid
traceparent values must be rejected consistently by request-ID derivation
and tracing: use the SDK parser instead of a partial handwritten parser.
`OTEL_ENABLED=false` installs a no-op tracer provider; metrics remain active.

### Log correlation

A slog handler enriches every `slog.*Context` call from context values with
request ID, authenticated user ID, and valid trace/span IDs. This includes
application/adapter logs: merely storing a logger in context would not
enrich calls to the global slog logger. Logger derivation with WithAttrs or
WithGroup preserves enrichment. No baggage or credentials are logged.

Nested response recorders carry trace context and user ID back to the outer
access logger, since contexts flow inward. Recovery obtains request ID from
the already-sanitized response header when its outer context predates ID
assignment. An access-log entry still correlates the panic with its trace.
Recorders handle informational headers, implicit 200 on flush, and expose
Unwrap for http.ResponseController.

### Metrics

Use the official Prometheus Go client with a PRIVATE registry per API process.
This avoids global collector conflicts, makes tests independent, and keeps
metrics available with tracing disabled. No OTel metric bridge is needed
for this pull-only deployment. See the [client documentation](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus).

`GET /metrics` exports Prometheus text, including Go/process collectors:

| Metric | Meaning |
|--------|---------|
| `booking_http_requests_total{method,route,status}` | Completed requests, including CORS/auth/rate rejections and panics |
| `booking_http_request_duration_seconds{method,route,status}` | Handler duration histogram in seconds |
| `booking_confirmations_total` | New booking+outbox commits observed in this process; excludes retries |
| `booking_payments_captured_total` | New durable capture transitions, from client confirmation or webhook |
| `booking_payment_capture_attempts_total{outcome}` | Actual client-initiated gateway capture calls; outcome = success/declined/error |
| `booking_active_holds` | Global Redis index members with expiry score strictly after the scrape time |
| `booking_active_holds_collection_success` | 1 for a successful Redis observation, 0 otherwise |

Store/gateway decorators implement existing application ports. Confirmation
increments only after successful Confirm (booking+outbox commit); capture
increments only when Capture returns changed=true. Concurrent retries and
duplicate webhooks therefore do not inflate durable-outcome counters. The
gateway-attempt counter deliberately counts calls rather than unique charges:
it is distinct from durable captures, and webhook-driven status changes do
not call the gateway. A failed gateway call and a captured-but-unbooked
payment remain different facts, not one generic payment-failure number.

HTTP labels contain only a finite method set (unknown tokens become OTHER),
registered route templates or `unmatched`, and status codes. No path IDs,
user IDs, session IDs, trace IDs or request IDs become metric labels.
Histogram buckets span 5ms–10s, covering fast catalog reads through expensive
password hashing and infrastructure timeouts. Metrics are never sampled.

Active holds use ZCOUNT over future expiry scores, excluding stale members
waiting for the sweeper. This is a global index observation, not a scan of
seat keys or a guarantee that every member has a live session hash. It uses
the existing second-resolution boundary (ADR 0010). Redis I/O is bounded
by `METRICS_REDIS_TIMEOUT` (default 1s). If it fails, omit the gauge and emit
collection_success=0; exporting zero would falsely claim no demand. Other
metrics remain available. At most two scrapes run concurrently.
Enable go-redis ContextTimeoutEnabled so the caller's deadline also bounds
socket I/O; passing a context alone would otherwise leave its background
socket timeout in control. A stalled local-socket test verifies this.

GET/HEAD /healthz, /readyz and /metrics do not consume rate quota, generate
HTTP traces/metrics or access logs. Their dependency reads also lack a
recording parent, avoiding trace noise from each scrape/probe. Other methods
on those paths still count as normal traffic. CORS remains outside routing.

## Consequences and limits

- A registry counter is operational evidence, not an accounting ledger.
  Counters reset on restart, and a crash after DB commit but before increment
  can undercount. Durable sales/payment tables remain the financial truth.
- The active-holds gauge reads SHARED Redis state: do not sum it across API
  replicas. Counters/histograms are local to each replica and may be summed
  with appropriate PromQL rate calculations. A stale confirmed hold after
  cleanup failure remains in this index gauge until cleanup/expiry.
- The API HTTP trace ends at the outbox commit. The outbox schema does not
  persist trace context, so a later relay tick starts a NEW trace; publisher
  and consumer are linked within that trace. Claiming a single HTTP→email
  trace would be misleading. Persisting trace context/links in outbox
  metadata is a future schema-level decision, not payload parsing in the
  generic relay. Booking/session/event IDs correlate durable records today.
- The worker emits traces and correlated logs but has no metrics listener
  in this slice. API metrics cover the requested HTTP and business signals;
  relay backlog/lag and notification delivery metrics are future additions.
- The metrics endpoint has no application auth; expose it only to the intended
  monitoring network at deployment. Bound scrape concurrency and Redis I/O.
- There is no bundled Prometheus server or dashboard yet: inspect /metrics
  directly or configure your own scraper. Jaeger stores traces, not these
  metrics. This keeps infrastructure focused on the signals being studied.
- Tests verify actual protobuf OTLP/HTTP export to a local receiver, export
  outage isolation, sampling, span parentage, safe labels/attributes, log
  correlation, concurrent counters, Redis expiry filtering and scrape failure.
  Real Redis coverage lives in the optional integration suite.
