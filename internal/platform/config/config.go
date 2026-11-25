// Package config loads the process configuration from environment variables
// and validates it at boot (CLAUDE.md §10).
//
// The design rule here is fail-fast: Load returns a joined error describing
// EVERY problem it found, and the process must exit non-zero rather than
// start half-configured. A process that boots in a broken state is worse
// than one that refuses to boot — the former masquerades as healthy in
// orchestrators and load balancers.
//
// Every variable is documented in .env.example. There are no defaults for
// values that differ per deployment (addresses, credentials): silently
// falling back to a default there is how a production process ends up
// talking to a development database.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment classifies the deployment target. Behavior that differs by
// environment (log format, secret strictness, migration automation) branches
// on this value instead of on scattered booleans.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvProduction  Environment = "production"
	EnvTest        Environment = "test"
)

// IsProduction reports whether the process runs in production. Used to make
// validation stricter where it matters (e.g. minimum JWT secret length).
func (e Environment) IsProduction() bool { return e == EnvProduction }

// PaymentProvider selects the payment gateway adapter (CLAUDE.md §7).
type PaymentProvider string

const (
	// PaymentProviderFake is the deterministic in-process sandbox. It is the
	// default for dev and the only provider automated tests may use.
	PaymentProviderFake PaymentProvider = "fake"
	// PaymentProviderStripe targets Stripe TEST mode. Live keys must never
	// be configured here; the adapter has no code path for live mode.
	PaymentProviderStripe PaymentProvider = "stripe"
)

// Config is the fully-validated process configuration. Construct it with
// Load; do not build it field-by-field in application code.
type Config struct {
	Env         Environment
	ServiceName string

	HTTP      HTTP
	CORS      CORS
	RateLimit RateLimit
	Log       Log
	Redis     Redis
	Booking   Booking
	Postgres  Postgres
	Auth      Auth
	Broker    Broker
	Worker    Worker
	Payment   Payment
	Telemetry Telemetry
}

// HTTP holds the API server settings.
type HTTP struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
}

// CORS permits exact browser origins. An empty list denies Origin-bearing
// requests; same-origin browser deployments must list their own origin too.
type CORS struct {
	AllowedOrigins []string
}

// BucketPolicy bounds the initial burst and continuous refill independently.
// RequestsPerMinute is a refill rate, not a fixed-window request count.
type BucketPolicy struct {
	RequestsPerMinute int
	Burst             int
}

// RateLimit is local to one API process. IdleTTL must allow every bucket to
// refill completely before eviction, so eviction cannot reset a depleted quota.
type RateLimit struct {
	General    BucketPolicy
	Auth       BucketPolicy
	Hold       BucketPolicy
	MaxClients int
	IdleTTL    time.Duration
}

// Log holds logging settings.
type Log struct {
	Level slog.Level
}

// Redis holds connection settings for the hold store. Redis carries only
// transient concurrency state (holds and session reverse-lookup keys).
type Redis struct {
	Addr     string
	Password string
	DB       int
	HoldTTL  time.Duration
}

// Booking holds booking policy enforced by the application layer.
type Booking struct {
	MaxActiveHolds int
}

// Postgres holds connection settings for the durable store.
type Postgres struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	SSLMode  string
}

// DSN renders the connection string in keyword/value form.
//
// Deliberately NOT "postgres://...": keyword/value keeps credentials out of
// URL-shaped strings so they cannot leak through URL-shaped logging, and it
// lets us lint the codebase for literal connection strings (scripts/lint-guards.sh).
func (p Postgres) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		p.Host, p.Port, p.User, p.Password, p.Database, p.SSLMode,
	)
}

// Auth holds authentication settings (CLAUDE.md §3).
type Auth struct {
	JWTSecret       string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
}

// Broker holds the RabbitMQ settings (CLAUDE.md §6, ADR 0009). URL is the
// connection string; the rest names the topology both the publisher and the
// consumer declare idempotently at every connect (see
// internal/adapters/messaging — declaring from BOTH sides closes the
// publish-before-queue-exists loss window).
type Broker struct {
	URL      string
	Exchange string
	Queue    string
	// Prefetch bounds a consumer's unacked in-flight deliveries (Qos).
	Prefetch int
}

// Worker holds the tuning knobs for cmd/worker's background jobs. The API
// validates these too (config.Load is shared) and ignores them — one
// validated config for every binary beats per-binary loaders at this scale;
// the alternative (splitting Load) is a refactor with no behavioral payoff.
type Worker struct {
	// RelayPollInterval is how often the outbox relay runs a pass. Polling
	// (vs LISTEN/NOTIFY) is the deliberate choice: it is stateless, survives
	// Postgres restarts without a notification channel to rebuild, and a
	// few seconds of publish latency is invisible next to a 5-minute hold
	// TTL (ADR 0009).
	RelayPollInterval time.Duration
	// RelayBatchSize bounds one relay pass (the SQL LIMIT).
	RelayBatchSize int
	// SweepInterval is how often the hold-expiry sweeper runs a pass.
	// 30s default: holds live minutes (REDIS_HOLD_TTL, default 5m), so
	// half-a-minute granularity on slot reclaim is invisible to users
	// while keeping the sweep a cheap bounded scan (ADR 0010).
	SweepInterval time.Duration
	// SweepBatchSize bounds one sweep pass (the ZSET window size).
	SweepBatchSize int
}

// Payment holds payment gateway selection and credentials (CLAUDE.md §7).
type Payment struct {
	Provider            PaymentProvider
	FakeWebhookSecret   string
	StripeSecretKey     string
	StripeWebhookSecret string
}

// Telemetry holds OpenTelemetry settings (CLAUDE.md §8).
type Telemetry struct {
	Enabled             bool
	Exporter            string
	OTLPEndpoint        string
	SampleRatio         float64
	ExportTimeout       time.Duration
	MetricsRedisTimeout time.Duration
}

// Load reads the environment, validates it, and returns the configuration or
// a joined error listing every problem found. Call it exactly once at boot.
func Load() (*Config, error) {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	cfg := &Config{}

	// -- process-wide settings -------------------------------------------------
	cfg.Env = Environment(withDefault("ENV", string(EnvDevelopment)))
	switch cfg.Env {
	case EnvDevelopment, EnvProduction, EnvTest:
	default:
		fail("ENV: %q is not one of development, production, test", cfg.Env)
	}

	cfg.ServiceName = withDefault("SERVICE_NAME", "booking-api")

	// -- HTTP -------------------------------------------------------------------
	cfg.HTTP.Addr = os.Getenv("HTTP_ADDR")
	if cfg.HTTP.Addr == "" {
		fail("HTTP_ADDR is required (e.g. :8080)")
	} else if !strings.Contains(cfg.HTTP.Addr, ":") {
		fail("HTTP_ADDR: %q must include a port (e.g. :8080)", cfg.HTTP.Addr)
	}
	cfg.HTTP.ReadTimeout = duration("HTTP_READ_TIMEOUT", 5*time.Second, fail)
	cfg.HTTP.WriteTimeout = duration("HTTP_WRITE_TIMEOUT", 10*time.Second, fail)
	cfg.HTTP.ShutdownTimeout = duration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second, fail)
	cfg.CORS.AllowedOrigins = corsOrigins(os.Getenv("CORS_ALLOWED_ORIGINS"), fail)
	cfg.RateLimit.General = bucketPolicy("RATE_LIMIT_GENERAL", 120, 30, fail)
	cfg.RateLimit.Auth = bucketPolicy("RATE_LIMIT_AUTH", 10, 5, fail)
	cfg.RateLimit.Hold = bucketPolicy("RATE_LIMIT_HOLD", 30, 5, fail)
	cfg.RateLimit.MaxClients = intInRange("RATE_LIMIT_MAX_CLIENTS", 10000, 1, 100000, fail)
	cfg.RateLimit.IdleTTL = duration("RATE_LIMIT_IDLE_TTL", 10*time.Minute, fail)
	for _, policy := range []BucketPolicy{cfg.RateLimit.General, cfg.RateLimit.Auth, cfg.RateLimit.Hold} {
		// Round UP: an entry must never expire before a fully depleted
		// bucket would naturally refill (including fractional nanoseconds).
		numerator := time.Minute * time.Duration(policy.Burst)
		refillTime := (numerator + time.Duration(policy.RequestsPerMinute) - 1) / time.Duration(policy.RequestsPerMinute)
		if cfg.RateLimit.IdleTTL < refillTime {
			fail("RATE_LIMIT_IDLE_TTL must allow all bursts to refill (need at least %s)", refillTime)
		}
	}

	// -- logging ------------------------------------------------------------------
	cfg.Log.Level = logLevel("LOG_LEVEL", fail)

	// -- Redis --------------------------------------------------------------------
	cfg.Redis.Addr = os.Getenv("REDIS_ADDR")
	if cfg.Redis.Addr == "" {
		fail("REDIS_ADDR is required (e.g. localhost:6379)")
	}
	cfg.Redis.Password = os.Getenv("REDIS_PASSWORD") // optional: dev Redis has no auth
	cfg.Redis.DB = intInRange("REDIS_DB", 0, 0, 15, fail)
	cfg.Redis.HoldTTL = duration("REDIS_HOLD_TTL", 5*time.Minute, fail)

	// -- booking policy -------------------------------------------------------------
	cfg.Booking.MaxActiveHolds = intInRange("MAX_ACTIVE_HOLDS", 4, 1, 100, fail)

	// -- Postgres ---------------------------------------------------------------------
	cfg.Postgres.Host = os.Getenv("POSTGRES_HOST")
	if cfg.Postgres.Host == "" {
		fail("POSTGRES_HOST is required")
	}
	cfg.Postgres.Port = intInRange("POSTGRES_PORT", 5432, 1, 65535, fail)
	cfg.Postgres.User = os.Getenv("POSTGRES_USER")
	if cfg.Postgres.User == "" {
		fail("POSTGRES_USER is required")
	}
	cfg.Postgres.Password = os.Getenv("POSTGRES_PASSWORD")
	if cfg.Postgres.Password == "" {
		fail("POSTGRES_PASSWORD is required (local dev: set any throwaway value in .env)")
	}
	cfg.Postgres.Database = os.Getenv("POSTGRES_DB")
	if cfg.Postgres.Database == "" {
		fail("POSTGRES_DB is required")
	}
	cfg.Postgres.SSLMode = withDefault("POSTGRES_SSLMODE", "disable")
	switch cfg.Postgres.SSLMode {
	case "disable", "require", "verify-ca", "verify-full":
	default:
		fail("POSTGRES_SSLMODE: %q is not one of disable, require, verify-ca, verify-full", cfg.Postgres.SSLMode)
	}

	// -- auth ---------------------------------------------------------------------------
	cfg.Auth.JWTSecret = os.Getenv("AUTH_JWT_SECRET")
	if cfg.Auth.JWTSecret == "" {
		fail("AUTH_JWT_SECRET is required (generate: openssl rand -hex 32)")
	} else if cfg.Env.IsProduction() && len(cfg.Auth.JWTSecret) < 32 {
		fail("AUTH_JWT_SECRET must be at least 32 characters in production")
	}
	cfg.Auth.AccessTokenTTL = duration("AUTH_ACCESS_TOKEN_TTL", 15*time.Minute, fail)
	cfg.Auth.RefreshTokenTTL = duration("AUTH_REFRESH_TOKEN_TTL", 720*time.Hour, fail)

	// -- broker ------------------------------------------------------------------------
	cfg.Broker.URL = os.Getenv("RABBITMQ_URL")
	if cfg.Broker.URL == "" {
		fail("RABBITMQ_URL is required (e.g. amqp://user:pass@localhost:5672/vhost)")
	} else if !strings.HasPrefix(cfg.Broker.URL, "amqp://") && !strings.HasPrefix(cfg.Broker.URL, "amqps://") {
		fail("RABBITMQ_URL must start with amqp:// or amqps://")
	}
	// Topology names have safe defaults (unlike addresses/credentials):
	// they identify OUR objects, not a deployment's, and every process
	// declares the topology idempotently — a mismatch against a live broker
	// fails the declare loudly instead of silently forking the topology.
	cfg.Broker.Exchange = withDefault("RABBITMQ_EXCHANGE", "booking.events")
	cfg.Broker.Queue = withDefault("RABBITMQ_QUEUE", "booking.notifications")
	cfg.Broker.Prefetch = intInRange("RABBITMQ_PREFETCH", 10, 1, 1000, fail)

	// -- worker ------------------------------------------------------------------------
	cfg.Worker.RelayPollInterval = duration("WORKER_RELAY_POLL_INTERVAL", 5*time.Second, fail)
	cfg.Worker.RelayBatchSize = intInRange("WORKER_RELAY_BATCH_SIZE", 100, 1, 10000, fail)
	cfg.Worker.SweepInterval = duration("WORKER_SWEEP_INTERVAL", 30*time.Second, fail)
	cfg.Worker.SweepBatchSize = intInRange("WORKER_SWEEP_BATCH_SIZE", 500, 1, 10000, fail)

	// -- payments ---------------------------------------------------------------------
	cfg.Payment.Provider = PaymentProvider(withDefault("PAYMENT_PROVIDER", string(PaymentProviderFake)))
	switch cfg.Payment.Provider {
	case PaymentProviderFake, PaymentProviderStripe:
	default:
		fail("PAYMENT_PROVIDER: %q is not one of fake, stripe", cfg.Payment.Provider)
	}
	if cfg.Payment.Provider == PaymentProviderFake {
		// The sandbox signs and verifies webhook payloads with an HMAC
		// secret (ADR 0008). It is a REAL secret in the only sense that
		// matters for the lesson: whoever holds it can forge captures, so
		// it is env-driven like every other credential (§10) and never
		// hardcoded or defaulted. Dev boxes generate one; CI pins its own.
		cfg.Payment.FakeWebhookSecret = os.Getenv("PAYMENT_FAKE_WEBHOOK_SECRET")
		if cfg.Payment.FakeWebhookSecret == "" {
			fail("PAYMENT_FAKE_WEBHOOK_SECRET is required when PAYMENT_PROVIDER=fake (generate: openssl rand -hex 32)")
		}
	}
	if cfg.Payment.Provider == PaymentProviderStripe {
		// Keys are validated here; the "stripe adapter not implemented yet"
		// failure belongs to the composition root, which owns adapter
		// selection (ADR 0008). A keyless stripe config still fails HERE
		// first — config checks shape, wiring checks capability.
		cfg.Payment.StripeSecretKey = os.Getenv("PAYMENT_STRIPE_SECRET_KEY")
		if cfg.Payment.StripeSecretKey == "" {
			fail("PAYMENT_STRIPE_SECRET_KEY is required when PAYMENT_PROVIDER=stripe")
		}
		cfg.Payment.StripeWebhookSecret = os.Getenv("PAYMENT_STRIPE_WEBHOOK_SECRET")
		if cfg.Payment.StripeWebhookSecret == "" {
			fail("PAYMENT_STRIPE_WEBHOOK_SECRET is required when PAYMENT_PROVIDER=stripe")
		}
	}

	// -- telemetry ---------------------------------------------------------------------
	cfg.Telemetry.Enabled = boolVar("OTEL_ENABLED", true, fail)
	cfg.Telemetry.Exporter = withDefault("OTEL_TRACES_EXPORTER", "otlp")
	if cfg.Telemetry.Exporter != "otlp" && cfg.Telemetry.Exporter != "stdout" {
		fail("OTEL_TRACES_EXPORTER must be otlp or stdout")
	}
	cfg.Telemetry.OTLPEndpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if cfg.Telemetry.OTLPEndpoint == "" {
		if cfg.Telemetry.Enabled && cfg.Telemetry.Exporter == "otlp" {
			fail("OTEL_EXPORTER_OTLP_ENDPOINT is required when tracing is enabled with OTEL_TRACES_EXPORTER=otlp")
		}
	} else {
		endpoint, err := url.Parse(cfg.Telemetry.OTLPEndpoint)
		if err != nil || endpoint == nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
			endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" ||
			(endpoint.Path != "" && endpoint.Path != "/") {
			fail("OTEL_EXPORTER_OTLP_ENDPOINT must be an http(s) base URL without credentials, query, fragment or path; OTLP/HTTP appends /v1/traces")
		} else {
			base := endpoint.Scheme + "://" + endpoint.Host
			if (cfg.Telemetry.OTLPEndpoint != base && cfg.Telemetry.OTLPEndpoint != base+"/") || strings.HasSuffix(endpoint.Host, ":") {
				fail("OTEL_EXPORTER_OTLP_ENDPOINT must be an http(s) base URL without query, fragment or empty port")
			}
			if port := endpoint.Port(); port != "" {
				value, err := strconv.Atoi(port)
				if err != nil || value < 1 || value > 65535 {
					fail("OTEL_EXPORTER_OTLP_ENDPOINT port must be between 1 and 65535")
				}
			}
		}
	}
	cfg.Telemetry.SampleRatio = 1
	if raw := os.Getenv("OTEL_TRACE_SAMPLE_RATIO"); raw != "" {
		ratio, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 || ratio > 1 {
			fail("OTEL_TRACE_SAMPLE_RATIO must be a finite number between 0 and 1")
		} else {
			cfg.Telemetry.SampleRatio = ratio
		}
	}
	cfg.Telemetry.ExportTimeout = duration("OTEL_EXPORT_TIMEOUT", 3*time.Second, fail)
	cfg.Telemetry.MetricsRedisTimeout = duration("METRICS_REDIS_TIMEOUT", time.Second, fail)

	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// withDefault returns the variable's value or def when unset/empty.
func withDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func bucketPolicy(prefix string, rpm, burst int, fail func(string, ...any)) BucketPolicy {
	return BucketPolicy{
		RequestsPerMinute: intInRange(prefix+"_RPM", rpm, 1, 1000000, fail),
		Burst:             intInRange(prefix+"_BURST", burst, 1, 100000, fail),
	}
}

func corsOrigins(raw string, fail func(string, ...any)) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var origins []string
	seen := make(map[string]bool)
	for _, value := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(value)
		u, err := url.Parse(origin)
		// Serialized origins have no credentials, path (even a trailing /),
		// query or fragment. Never accept opaque/null or wildcard origins
		// when allowing browser credentials (ADR 0011).
		if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") ||
			u.Hostname() == "" || u.User != nil || origin != u.Scheme+"://"+u.Host ||
			strings.ContainsAny(origin, "*\\\t\r\n ") {
			fail("CORS_ALLOWED_ORIGINS: %q must be an exact http(s) origin without path, credentials or wildcard", origin)
			continue
		}
		if strings.HasSuffix(u.Host, ":") {
			fail("CORS_ALLOWED_ORIGINS: %q has an empty port", origin)
			continue
		}
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 0 || n > 65535 {
				fail("CORS_ALLOWED_ORIGINS: %q has an invalid port", origin)
				continue
			}
		}
		if strings.HasPrefix(u.Host, "[") {
			ip, err := netip.ParseAddr(u.Hostname())
			if err != nil || !ip.Is6() || ip.Zone() != "" {
				fail("CORS_ALLOWED_ORIGINS: %q has an invalid IPv6 host", origin)
				continue
			}
		}
		if !seen[origin] {
			origins = append(origins, origin)
			seen[origin] = true
		}
	}
	return origins
}

// duration parses a Go duration variable, reporting through fail.
func duration(key string, def time.Duration, fail func(string, ...any)) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		fail("%s: %q is not a valid duration (%v)", key, raw, err)
		return def
	}
	if d <= 0 {
		fail("%s must be positive, got %s", key, d)
		return def
	}
	return d
}

// intInRange parses an integer variable bounded to [min, max].
func intInRange(key string, def, min, max int, fail func(string, ...any)) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		fail("%s: %q is not an integer", key, raw)
		return def
	}
	if n < min || n > max {
		fail("%s: %d is out of range [%d, %d]", key, n, min, max)
		return def
	}
	return n
}

// boolVar parses a boolean variable accepting 1/0/true/false/yes/no.
func boolVar(key string, def bool, fail func(string, ...any)) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	default:
		fail("%s: %q is not a valid boolean (use true/false)", key, raw)
		return def
	}
}

// logLevel parses LOG_LEVEL into a slog.Level.
func logLevel(key string, fail func(string, ...any)) slog.Level {
	switch strings.ToLower(withDefault(key, "info")) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		fail("%s: must be one of debug, info, warn, error", key)
		return slog.LevelInfo
	}
}
