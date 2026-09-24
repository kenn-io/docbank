package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/require"
)

func TestPassageCreateRegistrarAcceptsBoundedOwnerRequest(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("passage test", "1"))
	registerPassageCreateRoute(api, Deps{}, NewOperationGate())
	request := httptest.NewRequest(http.MethodPost, "/api/v1/passages/create", strings.NewReader(
		`{"node_id":1,"content_version_id":"version","rendition_build_id":"build","attachment_id":"attachment","byte_start":0,"byte_end":1}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())

	request = httptest.NewRequest(http.MethodPost, "/api/v1/passages/create", strings.NewReader(
		`{"node_id":0,"content_version_id":"version","rendition_build_id":"build","attachment_id":"attachment","byte_start":0,"byte_end":1}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	require.Equal(t, http.StatusUnprocessableEntity, response.Code, response.Body.String())
}
