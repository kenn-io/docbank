package api_test

import (
	"bytes"
	"encoding/json/jsontext"
	"net/http"
	"net/http/httptest"
	"testing"
)

type packageResponse struct {
	Code int
	Body *bytes.Buffer
}

type testServer struct {
	ts *httptest.Server
}

func newPackageTestServer(t *testing.T) (*testServer, *testStore) {
	t.Helper()
	ts, store := newTestServer(t, nil)
	return &testServer{ts: ts}, store
}

func (s *testServer) call(t *testing.T, method, path, body string, hdr map[string]string) packageResponse {
	t.Helper()
	var payload any
	if body != "" {
		payload = jsontext.Value(body)
	}
	resp, responseBody := do(t, s.ts, method, path, hdr, payload)
	return packageResponse{Code: resp.StatusCode, Body: bytes.NewBufferString(responseBody)}
}

func (s *testServer) post(t *testing.T, path, body string) packageResponse {
	t.Helper()
	return s.call(t, http.MethodPost, path, body, nil)
}

func (s *testServer) get(t *testing.T, path string) packageResponse {
	t.Helper()
	return s.call(t, http.MethodGet, path, "", nil)
}
