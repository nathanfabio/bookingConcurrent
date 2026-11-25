package config

import (
	"strings"
	"testing"
	"time"
)

func TestTelemetryConfiguration(t *testing.T) {
	setValidEnv(t)
	for _, key := range []string{"OTEL_TRACES_EXPORTER", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_TRACE_SAMPLE_RATIO", "OTEL_EXPORT_TIMEOUT", "METRICS_REDIS_TIMEOUT"} {
		t.Setenv(key, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry.Exporter != "otlp" || cfg.Telemetry.OTLPEndpoint != "" ||
		cfg.Telemetry.SampleRatio != 1 || cfg.Telemetry.ExportTimeout != 3*time.Second || cfg.Telemetry.MetricsRedisTimeout != time.Second {
		t.Fatalf("telemetry defaults: %+v", cfg.Telemetry)
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "stdout")
	t.Setenv("OTEL_TRACE_SAMPLE_RATIO", "0")
	cfg, err = Load()
	if err != nil || cfg.Telemetry.Exporter != "stdout" || cfg.Telemetry.SampleRatio != 0 {
		t.Fatalf("stdout/explicit zero sampling: %+v, %v", cfg, err)
	}
}

func TestOTLPEndpointRequiredOnlyWhenUsed(t *testing.T) {
	setValidEnv(t)
	t.Setenv("OTEL_ENABLED", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "OTEL_EXPORTER_OTLP_ENDPOINT") {
		t.Fatalf("enabled OTLP requires an explicit deployment address: %v", err)
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "stdout")
	if _, err := Load(); err != nil {
		t.Fatalf("stdout needs no network endpoint: %v", err)
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_ENABLED", "false")
	if _, err := Load(); err != nil {
		t.Fatalf("disabled tracing needs no network endpoint: %v", err)
	}
}

func TestInvalidTelemetryConfiguration(t *testing.T) {
	for key, values := range map[string][]string{
		"OTEL_TRACES_EXPORTER":        {"grpc", "unknown"},
		"OTEL_EXPORTER_OTLP_ENDPOINT": {"localhost:4318", "/relative", "ftp://localhost:4318", "http://", "http://user:secret@localhost:4318", "http://localhost:4318/v1/traces", "http://localhost:4318?secret=x", "http://localhost:4318?", "http://localhost:4318#fragment", "http://localhost:4318#", "http://localhost:", "http://localhost:0", "http://localhost:65536"},
		"OTEL_TRACE_SAMPLE_RATIO":     {"NaN", "Inf", "-0.1", "1.1", "all"},
		"OTEL_EXPORT_TIMEOUT":         {"0s", "-1s", "later"},
		"METRICS_REDIS_TIMEOUT":       {"0s", "-1s", "later"},
	} {
		for _, value := range values {
			t.Run(key+"/"+value, func(t *testing.T) {
				setValidEnv(t)
				t.Setenv(key, value)
				_, err := Load()
				if err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("%s=%q: %v, want validation error", key, value, err)
				}
			})
		}
	}
}
