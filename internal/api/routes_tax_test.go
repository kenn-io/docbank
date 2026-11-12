package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
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
