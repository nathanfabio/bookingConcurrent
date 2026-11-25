package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestContextCorrelationSurvivesLoggerDerivation(t *testing.T) {
	var output bytes.Buffer
	log := slog.New(WithCorrelation(slog.NewJSONHandler(&output, nil)))
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}})
	ctx := ContextWithUserID(ContextWithRequestID(context.Background(), "request-123"), "alice")
	ctx = trace.ContextWithSpanContext(ctx, sc)
	log.With("component", "application").InfoContext(ctx, "application message", "request_id", "request-123")
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"request_id": "request-123", "user_id": "alice", "trace_id": sc.TraceID().String(), "span_id": sc.SpanID().String(), "component": "application",
	} {
		if record[key] != value {
			t.Errorf("%s=%v, want %s", key, record[key], value)
		}
	}
	if strings.Count(output.String(), `"request_id"`) != 1 {
		t.Error("explicit correlation attributes must not be duplicated")
	}
	output.Reset()
	log.WithGroup("job").With("name", "relay").InfoContext(ctx, "worker message")
	if !strings.Contains(output.String(), `"trace_id"`) || !strings.Contains(output.String(), `"request_id"`) {
		t.Error("WithGroup/WithAttrs dropped correlation handler")
	}
	output.Reset()
	log.InfoContext(context.Background(), "boot")
	if strings.Contains(output.String(), "request_id") || strings.Contains(output.String(), "trace_id") || strings.Contains(output.String(), "user_id") {
		t.Error("background records must not inherit prior request metadata")
	}
}
