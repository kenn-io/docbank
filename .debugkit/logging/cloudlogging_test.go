package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

func TestCloudLoggingFormatWritesSpecialFields(t *testing.T) {
	t.Parallel()

	traceID := trace.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
	spanID := trace.SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7}
	ctx := trace.ContextWithSpanContext(t.Context(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	tests := []struct {
		name  string
		group string
	}{
		{name: "top level"},
		{name: "inside group", group: "request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			logger, _, err := NewLogger(Options{Stderr: &out, Format: "gcp", AddSource: true})
			require.NoError(t, err)
			logger = logger.With("component", "hub")
			if tt.group != "" {
				logger = logger.WithGroup(tt.group)
			}
			logger.WarnContext(ctx, "renewal failed", "attempt", 3)

			var entry map[string]any
			require.NoError(t, json.Unmarshal(out.Bytes(), &entry), out.String())

			assert.Equal(t, "WARNING", entry["severity"])
			assert.Equal(t, "renewal failed", entry["message"])
			assert.NotContains(t, entry, "level")
			assert.NotContains(t, entry, "msg")
			timestamp, ok := entry["time"].(string)
			require.True(t, ok, "time must be a string: %v", entry["time"])
			_, err = time.Parse(time.RFC3339Nano, timestamp)
			require.NoError(t, err)

			assert.Equal(t, traceID.String(), entry["logging.googleapis.com/trace"])
			assert.Equal(t, spanID.String(), entry["logging.googleapis.com/spanId"])
			assert.Equal(t, true, entry["logging.googleapis.com/trace_sampled"])

			location, ok := entry["logging.googleapis.com/sourceLocation"].(map[string]any)
			require.True(t, ok, "sourceLocation must be an object: %v", entry)
			file, _ := location["file"].(string)
			assert.True(t, strings.HasSuffix(file, "cloudlogging_test.go"), file)
			line, _ := location["line"].(string)
			lineNumber, err := strconv.Atoi(line)
			require.NoError(t, err, "line must be a decimal string")
			assert.Positive(t, lineNumber)
			assert.Contains(t, location["function"], "TestCloudLoggingFormatWritesSpecialFields")

			assert.Equal(t, "hub", entry["component"])
			attrs := entry
			if tt.group != "" {
				attrs, ok = entry[tt.group].(map[string]any)
				require.True(t, ok, "group %q must be an object: %v", tt.group, entry)
			}
			assert.InDelta(t, 3, attrs["attempt"], 0)
		})
	}
}

func TestCloudLoggingHandlerOmitsTraceWithoutSpan(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	slog.New(NewCloudLoggingHandler(&out, nil)).InfoContext(t.Context(), "started")

	var entry map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &entry), out.String())
	assert.Equal(t, "INFO", entry["severity"])
	assert.NotContains(t, entry, "logging.googleapis.com/trace")
	assert.NotContains(t, entry, "logging.googleapis.com/spanId")
	assert.NotContains(t, entry, "logging.googleapis.com/trace_sampled")
}

func TestCloudSeverity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		level slog.Level
		want  string
	}{
		{level: slog.LevelDebug - 4, want: "DEBUG"},
		{level: slog.LevelDebug, want: "DEBUG"},
		{level: slog.LevelInfo, want: "INFO"},
		{level: slog.LevelInfo + 2, want: "INFO"},
		{level: slog.LevelWarn, want: "WARNING"},
		{level: slog.LevelError, want: "ERROR"},
		{level: slog.LevelError + 4, want: "ERROR"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, cloudSeverity(tt.level), "level %v", tt.level)
	}
}
