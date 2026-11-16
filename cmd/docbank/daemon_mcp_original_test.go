package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDaemonMCPOriginalRoute(t *testing.T) {
	handler := withMCPRoute(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://docbank:7341/mcp/original?ticket=synthetic", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://docbank:7341/api/v1/info", nil))
	require.Equal(t, http.StatusForbidden, response.Code)
}
