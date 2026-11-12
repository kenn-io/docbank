package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaxValuesAbsentFromLogsAndTelemetry(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	const sentinel = "private-synthetic-value"
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(sentinel) })
	handler := logMiddleware(recoverMiddleware(next, logger), logger)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tax/accounts/"+sentinel+"?country="+sentinel, nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	assert.NotContains(t, logs.String(), sentinel)
}

func TestTaxLoggingPreservesOtherRoutes(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	handler := logMiddleware(recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("ordinary panic") }), logger), logger)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/other", nil))
	assert.Contains(t, logs.String(), "/api/v1/other")
	assert.Contains(t, logs.String(), "ordinary panic")
}

func TestTaxFormatterPreservesMediaValidation(t *testing.T) {
	const secret = "private-synthetic-media-url"
	problem := huma.NewError(http.StatusUnprocessableEntity, "validation", &huma.ErrorDetail{Location: "body.canonical_url", Value: secret, Message: "invalid"})
	var result *Error
	require.ErrorAs(t, problem, &result)
	assert.Equal(t, []string{"invalid media reference body"}, result.Errors)
	assert.NotContains(t, result.Error(), secret)
}

func TestTaxMutationCanceledAtGate(t *testing.T) {
	gate := NewOperationGate()
	require.NoError(t, gate.mu.Acquire(t.Context(), operationGateExclusiveWeight))
	defer gate.mu.Release(operationGateExclusiveWeight)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	err := gate.mutate(ctx, func() error { called = true; return nil })
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, called)
	assert.NotContains(t, taxRouteError(err).Error(), "context canceled")
}
