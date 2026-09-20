package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// setValidEnv populates every required variable with valid values, then a
// test mutates one aspect of the environment and asserts on the outcome.
func setValidEnv(t *testing.T) {
	t.Helper()
	vars := map[string]string{
		"ENV":                           "development",
		"SERVICE_NAME":                  "booking-api",
		"HTTP_ADDR":                     ":8080",
		"HTTP_READ_TIMEOUT":             "5s",
		"HTTP_WRITE_TIMEOUT":            "10s",
		"HTTP_SHUTDOWN_TIMEOUT":         "15s",
		"LOG_LEVEL":                     "info",
		"REDIS_ADDR":                    "localhost:6379",
		"REDIS_DB":                      "0",
		"REDIS_HOLD_TTL":                "5m",
		"MAX_ACTIVE_HOLDS":              "4",
		"POSTGRES_HOST":                 "localhost",
		"POSTGRES_PORT":                 "5432",
		"POSTGRES_USER":                 "booking",
		"POSTGRES_PASSWORD":             "booking-dev-password",
		"POSTGRES_DB":                   "booking",
		"POSTGRES_SSLMODE":              "disable",
		"AUTH_JWT_SECRET":               strings.Repeat("s", 32),
		"AUTH_ACCESS_TOKEN_TTL":         "15m",
		"AUTH_REFRESH_TOKEN_TTL":        "720h",
		"RABBITMQ_URL":                  "amqp://booking:pw@localhost:5672/booking",
		"PAYMENT_PROVIDER":              "fake",
		"PAYMENT_FAKE_WEBHOOK_SECRET":   strings.Repeat("w", 32),
		"PAYMENT_STRIPE_SECRET_KEY":     "",
		"PAYMENT_STRIPE_WEBHOOK_SECRET": "",
		"OTEL_ENABLED":                  "false",
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

func TestLoadValid(t *testing.T) {
	setValidEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error for valid env: %v", err)
	}

	if cfg.Env != EnvDevelopment {
		t.Errorf("Env = %q, want development", cfg.Env)
	}
	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("HTTP.Addr = %q, want :8080", cfg.HTTP.Addr)
	}
	if cfg.Redis.HoldTTL != 5*time.Minute {
		t.Errorf("Redis.HoldTTL = %s, want 5m", cfg.Redis.HoldTTL)
	}
	if cfg.Booking.MaxActiveHolds != 4 {
		t.Errorf("Booking.MaxActiveHolds = %d, want 4", cfg.Booking.MaxActiveHolds)
	}
	if cfg.Log.Level != slog.LevelInfo {
		t.Errorf("Log.Level = %v, want info", cfg.Log.Level)
	}
	if cfg.Payment.Provider != PaymentProviderFake {
		t.Errorf("Payment.Provider = %q, want fake", cfg.Payment.Provider)
	}
	if cfg.Payment.FakeWebhookSecret == "" {
		t.Error("Payment.FakeWebhookSecret not loaded from env")
	}
	if cfg.Telemetry.Enabled {
		t.Errorf("Telemetry.Enabled = true, want false")
	}
}

func TestLoadDefaults(t *testing.T) {
	setValidEnv(t)
	// Remove everything that has a documented default.
	for _, k := range []string{
		"ENV", "SERVICE_NAME", "HTTP_READ_TIMEOUT", "HTTP_WRITE_TIMEOUT",
		"HTTP_SHUTDOWN_TIMEOUT", "LOG_LEVEL", "REDIS_DB", "REDIS_HOLD_TTL",
		"MAX_ACTIVE_HOLDS", "POSTGRES_SSLMODE", "PAYMENT_PROVIDER", "OTEL_ENABLED",
	} {
		t.Setenv(k, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Env != EnvDevelopment {
		t.Errorf("default Env = %q, want development", cfg.Env)
	}
	if cfg.ServiceName != "booking-api" {
		t.Errorf("default ServiceName = %q, want booking-api", cfg.ServiceName)
	}
	if cfg.Redis.HoldTTL != 5*time.Minute {
		t.Errorf("default HoldTTL = %s, want 5m", cfg.Redis.HoldTTL)
	}
	if cfg.Booking.MaxActiveHolds != 4 {
		t.Errorf("default MaxActiveHolds = %d, want 4", cfg.Booking.MaxActiveHolds)
	}
	if cfg.Payment.Provider != PaymentProviderFake {
		t.Errorf("default Payment.Provider = %q, want fake", cfg.Payment.Provider)
	}
	if !cfg.Telemetry.Enabled {
		t.Errorf("default Telemetry.Enabled = false, want true")
	}
}

func TestLoadMissingRequired(t *testing.T) {
	// Each required variable, when absent, must produce an error that names it.
	required := []string{
		"HTTP_ADDR",
		"REDIS_ADDR",
		"POSTGRES_HOST", "POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB",
		"AUTH_JWT_SECRET",
		"RABBITMQ_URL",
	}
	for _, key := range required {
		t.Run(key, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(key, "")

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with %s unset: expected error, got nil", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error should name the offending variable %s, got: %v", key, err)
			}
		})
	}
}

func TestLoadInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
		want string // substring expected in the error
	}{
		{"bad env", "ENV", "staging", "ENV"},
		{"bad log level", "LOG_LEVEL", "verbose", "LOG_LEVEL"},
		{"bad duration", "REDIS_HOLD_TTL", "five minutes", "REDIS_HOLD_TTL"},
		{"negative duration", "HTTP_READ_TIMEOUT", "-1s", "HTTP_READ_TIMEOUT"},
		{"bad int", "POSTGRES_PORT", "not-a-port", "POSTGRES_PORT"},
		{"port out of range", "POSTGRES_PORT", "99999", "POSTGRES_PORT"},
		{"zero holds", "MAX_ACTIVE_HOLDS", "0", "MAX_ACTIVE_HOLDS"},
		{"bad sslmode", "POSTGRES_SSLMODE", "yolo", "POSTGRES_SSLMODE"},
		{"bad provider", "PAYMENT_PROVIDER", "paypal", "PAYMENT_PROVIDER"},
		{"bad bool", "OTEL_ENABLED", "sometimes", "OTEL_ENABLED"},
		{"bad broker scheme", "RABBITMQ_URL", "http://nope", "RABBITMQ_URL"},
		{"http addr without port", "HTTP_ADDR", "localhost", "HTTP_ADDR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(tc.key, tc.val)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with %s=%q: expected error", tc.key, tc.val)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestLoadProductionSecretLength(t *testing.T) {
	setValidEnv(t)
	t.Setenv("ENV", "production")
	t.Setenv("AUTH_JWT_SECRET", "tooshort")

	_, err := Load()
	if err == nil {
		t.Fatal("production with a short JWT secret must fail validation")
	}
	if !strings.Contains(err.Error(), "AUTH_JWT_SECRET") {
		t.Errorf("error should name AUTH_JWT_SECRET, got: %v", err)
	}
}

func TestLoadStripeRequiresKeys(t *testing.T) {
	setValidEnv(t)
	t.Setenv("PAYMENT_PROVIDER", "stripe")

	_, err := Load()
	if err == nil {
		t.Fatal("stripe provider without keys must fail validation")
	}
	for _, key := range []string{"PAYMENT_STRIPE_SECRET_KEY", "PAYMENT_STRIPE_WEBHOOK_SECRET"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error should name %s, got: %v", key, err)
		}
	}
}

func TestLoadFakeRequiresWebhookSecret(t *testing.T) {
	setValidEnv(t)
	t.Setenv("PAYMENT_FAKE_WEBHOOK_SECRET", "")

	// The fake gateway signs/verifies webhook HMACs with this secret; an
	// unset secret must fail at boot, not silently disable verification
	// (fail-fast, CLAUDE.md §10).
	_, err := Load()
	if err == nil {
		t.Fatal("fake provider without a webhook secret must fail validation")
	}
	if !strings.Contains(err.Error(), "PAYMENT_FAKE_WEBHOOK_SECRET") {
		t.Errorf("error should name PAYMENT_FAKE_WEBHOOK_SECRET, got: %v", err)
	}
}

func TestLoadCollectsAllErrors(t *testing.T) {
	setValidEnv(t)
	t.Setenv("HTTP_ADDR", "")
	t.Setenv("REDIS_ADDR", "")
	t.Setenv("POSTGRES_PORT", "nope")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	// Fail-fast means reporting everything at once: one boot, full list.
	for _, key := range []string{"HTTP_ADDR", "REDIS_ADDR", "POSTGRES_PORT"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("joined error should include %s, got: %v", key, err)
		}
	}
}

func TestPostgresDSNKeywordValue(t *testing.T) {
	p := Postgres{Host: "h", Port: 5432, User: "u", Password: "p", Database: "d", SSLMode: "disable"}
	dsn := p.DSN()
	if strings.Contains(dsn, "postgres://") {
		t.Errorf("DSN must not use URL scheme (kept grep-able by lint guards): %s", dsn)
	}
	if !strings.Contains(dsn, "host=h") || !strings.Contains(dsn, "port=5432") {
		t.Errorf("DSN missing fields: %s", dsn)
	}
}
