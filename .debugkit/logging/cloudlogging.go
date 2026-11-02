package logging

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strconv"

	"go.opentelemetry.io/otel/trace"
)

// Google Cloud Logging special JSON fields. The logging agents in Cloud Run,
// GKE, and Compute Engine move these from jsonPayload into the LogEntry.
// See https://docs.cloud.google.com/logging/docs/agent/logging/configuration#special-fields.
const (
	cloudSeverityKey       = "severity"
	cloudMessageKey        = "message"
	cloudSourceLocationKey = "logging.googleapis.com/sourceLocation"
	cloudTraceKey          = "logging.googleapis.com/trace"
	cloudSpanIDKey         = "logging.googleapis.com/spanId"
	cloudTraceSampledKey   = "logging.googleapis.com/trace_sampled"
)

// NewCloudLoggingHandler returns a JSON handler whose records Google Cloud
// Logging parses into structured log entries: severity, message, time,
// source location, and the trace and span of the OpenTelemetry span in the
// record's context. It only formats output; it does not call any Google
// Cloud API, so it works anywhere.
//
// Trace fields stay at the top level even under WithGroup, because Cloud
// Logging reads them only there.
func NewCloudLoggingHandler(w io.Writer, opts *slog.HandlerOptions) slog.Handler {
	var handlerOpts slog.HandlerOptions
	if opts != nil {
		handlerOpts = *opts
	}
	callerReplace := handlerOpts.ReplaceAttr
	handlerOpts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
		if callerReplace != nil {
			a = callerReplace(groups, a)
		}
		if len(groups) > 0 {
			return a
		}
		return cloudLoggingAttr(a)
	}
	root := slog.NewJSONHandler(w, &handlerOpts)
	return &cloudLoggingHandler{root: root, scoped: root}
}

func cloudLoggingAttr(a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.LevelKey:
		if level, ok := a.Value.Any().(slog.Level); ok {
			return slog.String(cloudSeverityKey, cloudSeverity(level))
		}
	case slog.MessageKey:
		return slog.Attr{Key: cloudMessageKey, Value: a.Value}
	case slog.SourceKey:
		if source, ok := a.Value.Any().(*slog.Source); ok {
			// LogEntrySourceLocation encodes line as an int64 string.
			return slog.Group(cloudSourceLocationKey,
				slog.String("file", source.File),
				slog.String("line", strconv.Itoa(source.Line)),
				slog.String("function", source.Function),
			)
		}
	}
	return a
}

func cloudSeverity(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return "DEBUG"
	case level < slog.LevelWarn:
		return "INFO"
	case level < slog.LevelError:
		return "WARNING"
	default:
		return "ERROR"
	}
}

type cloudLoggingHandler struct {
	// root has no attrs or groups; scoped is root after every WithAttrs and
	// WithGroup call, which scopes records in order so grouped records can
	// be rebuilt with top-level trace fields.
	root    slog.Handler
	scoped  slog.Handler
	scopes  []cloudLoggingScope
	grouped bool
}

type cloudLoggingScope struct {
	group string
	attrs []slog.Attr
}

func (h *cloudLoggingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.scoped.Enabled(ctx, level)
}

func (h *cloudLoggingHandler) Handle(ctx context.Context, record slog.Record) error {
	span := trace.SpanContextFromContext(ctx)
	if !span.IsValid() {
		return h.scoped.Handle(ctx, record)
	}
	traceAttrs := []slog.Attr{
		slog.String(cloudTraceKey, span.TraceID().String()),
		slog.String(cloudSpanIDKey, span.SpanID().String()),
		slog.Bool(cloudTraceSampledKey, span.IsSampled()),
	}
	if !h.grouped {
		record = record.Clone()
		record.AddAttrs(traceAttrs...)
		return h.scoped.Handle(ctx, record)
	}
	handler := h.root.WithAttrs(traceAttrs)
	for _, scope := range h.scopes {
		if scope.group != "" {
			handler = handler.WithGroup(scope.group)
		} else {
			handler = handler.WithAttrs(scope.attrs)
		}
	}
	return handler.Handle(ctx, record)
}

func (h *cloudLoggingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &cloudLoggingHandler{
		root:    h.root,
		scoped:  h.scoped.WithAttrs(attrs),
		scopes:  append(slices.Clip(h.scopes), cloudLoggingScope{attrs: attrs}),
		grouped: h.grouped,
	}
}

func (h *cloudLoggingHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &cloudLoggingHandler{
		root:    h.root,
		scoped:  h.scoped.WithGroup(name),
		scopes:  append(slices.Clip(h.scopes), cloudLoggingScope{group: name}),
		grouped: true,
	}
}
