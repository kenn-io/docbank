package api

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestTaxValuesAbsentFromLogs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	const sentinel = "private-synthetic-value"
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(sentinel) })
	handler := logMiddleware(recoverMiddleware(next, logger), logger)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tax/accounts/"+sentinel+"?country="+sentinel, nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	assert.NotContains(t, logs.String(), sentinel)
	assert.Equal(t, "/api/v1/tax/{redacted}", requestLogPath("/api/v1/tax/"+sentinel))
}

func TestTaxLoggingPreservesOtherRoutes(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	handler := logMiddleware(recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("ordinary panic") }), logger), logger)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/other", nil))
	assert.Contains(t, logs.String(), "/api/v1/other")
	assert.Contains(t, logs.String(), "ordinary panic")
}

func TestTaxRevisionExhaustionError(t *testing.T) {
	var problem *Error
	require.ErrorAs(t, taxRouteError(store.ErrTaxRevisionExhausted), &problem)
	assert.Equal(t, http.StatusConflict, problem.Status)
	assert.Equal(t, "tax_revision_exhausted", problem.Code)
	assert.Equal(t, "tax request failed", problem.Detail)
}

func TestTaxFormatterPreservesMediaValidation(t *testing.T) {
	const secret = "private-synthetic-media-url"
	ctx := humago.NewContext(&huma.Operation{Path: "/api/v1/media"}, httptest.NewRequest(http.MethodPost, "/api/v1/media", nil), httptest.NewRecorder())
	problem := huma.NewErrorWithContext(ctx, http.StatusUnprocessableEntity, "validation", &huma.ErrorDetail{Location: "body.canonical_url", Value: secret, Message: "invalid"})
	var result *Error
	require.ErrorAs(t, problem, &result)
	assert.Equal(t, []string{"invalid media reference body"}, result.Errors)
	assert.NotContains(t, result.Error(), secret)
}

type failingTaxResponse struct{ *httptest.ResponseRecorder }

func (w failingTaxResponse) Write([]byte) (int, error) {
	return 0, errors.New("synthetic write failure")
}

func TestTaxOversizeWriteFailureOmitsPath(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	const sentinel = "private-synthetic-value"
	request := httptest.NewRequest(http.MethodPost, "/api/v1/tax/accounts/"+sentinel, nil)
	request.ContentLength = 10
	ctx := humago.NewContext(&huma.Operation{Path: "/api/v1/tax/accounts/{account_id}", MaxBodyBytes: 1}, request, failingTaxResponse{httptest.NewRecorder()})
	called := false
	rejectDeclaredOversizeBodies(NewOfflineServer().API(), logger)(ctx, func(huma.Context) { called = true })
	assert.False(t, called)
	assert.Contains(t, logs.String(), "write oversize body rejection")
	assert.Contains(t, logs.String(), "/api/v1/tax/{redacted}")
	assert.NotContains(t, logs.String(), sentinel)
}
