package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadHTTPProtectionDefaults(t *testing.T) {
	setValidEnv(t)
	for _, key := range []string{
		"CORS_ALLOWED_ORIGINS", "RATE_LIMIT_GENERAL_RPM", "RATE_LIMIT_GENERAL_BURST",
		"RATE_LIMIT_AUTH_RPM", "RATE_LIMIT_AUTH_BURST", "RATE_LIMIT_HOLD_RPM",
		"RATE_LIMIT_HOLD_BURST", "RATE_LIMIT_MAX_CLIENTS", "RATE_LIMIT_IDLE_TTL",
	} {
		t.Setenv(key, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := RateLimit{
		General:    BucketPolicy{RequestsPerMinute: 120, Burst: 30},
		Auth:       BucketPolicy{RequestsPerMinute: 10, Burst: 5},
		Hold:       BucketPolicy{RequestsPerMinute: 30, Burst: 5},
		MaxClients: 10000, IdleTTL: 10 * time.Minute,
	}
	if cfg.RateLimit != want || len(cfg.CORS.AllowedOrigins) != 0 {
		t.Fatalf("defaults: %+v, origins=%v", cfg.RateLimit, cfg.CORS.AllowedOrigins)
	}
}

func TestLoadCORSOrigins(t *testing.T) {
	setValidEnv(t)
	t.Setenv("CORS_ALLOWED_ORIGINS", " https://cinema.example , http://localhost:5173,https://cinema.example,http://[::1]:8080 ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://cinema.example", "http://localhost:5173", "http://[::1]:8080"}
	if !reflect.DeepEqual(cfg.CORS.AllowedOrigins, want) {
		t.Fatalf("origins=%v, want %v", cfg.CORS.AllowedOrigins, want)
	}
}

func TestLoadRejectsInvalidOrigins(t *testing.T) {
	for _, origin := range []string{
		"*", "null", "https://*.example", "https://cinema.example/", "https://cinema.example/path",
		"https://cinema.example?", "https://cinema.example#", "https://user:pw@cinema.example",
		"cinema.example", "ftp://cinema.example", "https://", "https://cinema.example:port",
		"https://cinema.example:65536", "https://cinema.example:", "https://[invalid]:8080",
		"http://[fe80::1%25eth0]:8080",
		"https://cinema.example,", "https://cinema.example\r\nInjected: yes", "https://two origins.example",
	} {
		t.Run(origin, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("CORS_ALLOWED_ORIGINS", origin)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "CORS_ALLOWED_ORIGINS") {
				t.Fatalf("origin %q: %v; want validation error", origin, err)
			}
		})
	}
}

func TestLoadRatePolicyOverrides(t *testing.T) {
	setValidEnv(t)
	values := map[string]string{
		"RATE_LIMIT_GENERAL_RPM": "100", "RATE_LIMIT_GENERAL_BURST": "20",
		"RATE_LIMIT_AUTH_RPM": "5", "RATE_LIMIT_AUTH_BURST": "1",
		"RATE_LIMIT_HOLD_RPM": "20", "RATE_LIMIT_HOLD_BURST": "2",
		"RATE_LIMIT_MAX_CLIENTS": "500", "RATE_LIMIT_IDLE_TTL": "1m",
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := RateLimit{
		General:    BucketPolicy{RequestsPerMinute: 100, Burst: 20},
		Auth:       BucketPolicy{RequestsPerMinute: 5, Burst: 1},
		Hold:       BucketPolicy{RequestsPerMinute: 20, Burst: 2},
		MaxClients: 500, IdleTTL: time.Minute,
	}
	if cfg.RateLimit != want {
		t.Fatalf("policy=%+v, want %+v", cfg.RateLimit, want)
	}
}

func TestLoadRejectsInvalidRatePolicy(t *testing.T) {
	for key, values := range map[string][]string{
		"RATE_LIMIT_GENERAL_RPM":   {"0", "-1", "1000001", "nan"},
		"RATE_LIMIT_GENERAL_BURST": {"0", "100001"},
		"RATE_LIMIT_AUTH_RPM":      {"0"},
		"RATE_LIMIT_AUTH_BURST":    {"0"},
		"RATE_LIMIT_HOLD_RPM":      {"0"},
		"RATE_LIMIT_HOLD_BURST":    {"0"},
		"RATE_LIMIT_MAX_CLIENTS":   {"0", "100001"},
		"RATE_LIMIT_IDLE_TTL":      {"0s", "-1m", "later", "1s"},
	} {
		for _, value := range values {
			t.Run(key+"/"+value, func(t *testing.T) {
				setValidEnv(t)
				t.Setenv(key, value)
				_, err := Load()
				if err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("%s=%s: %v; want validation error", key, value, err)
				}
			})
		}
	}
}

func TestLoadIdleTTLMustNotResetBucketsEarly(t *testing.T) {
	for _, prefix := range []string{"RATE_LIMIT_GENERAL", "RATE_LIMIT_AUTH", "RATE_LIMIT_HOLD"} {
		t.Run(prefix, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(prefix+"_RPM", "1")
			t.Setenv(prefix+"_BURST", "11")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "RATE_LIMIT_IDLE_TTL") {
				t.Fatalf("TTL shorter than refill must fail: %v", err)
			}
			t.Setenv("RATE_LIMIT_IDLE_TTL", "11m")
			if _, err := Load(); err != nil {
				t.Fatalf("inclusive refill boundary must work: %v", err)
			}
		})
	}
}
