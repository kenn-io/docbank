package api_test

import (
	"encoding/json/v2"
	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
	"net/http"
	"strings"
	"testing"
)

func TestMailboxAPIAuthenticatesAndFencesDeclaredChunkBytes(t *testing.T) {
	ts, s := newTestServer(t, nil)
	r := api.MailboxContainerInput{ID: "synthetic-mailbox", SHA256: strings.Repeat("a", 64), Size: 3, Format: "mbox"}
	response, body := do(t, ts, http.MethodPost, "/api/v1/mailbox/containers", nil, r)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var c store.MailboxContainer
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	require.Equal(t, "vault:"+s.VaultID(), c.Owner)
	response, body = do(t, ts, http.MethodPost, "/api/v1/mailbox/containers", map[string]string{"X-Api-Key": ""}, r)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/mailbox/containers/synthetic-mailbox/seal", nil, struct{}{})
	require.Equal(t, 422, response.StatusCode, body)
	response, body = do(t, ts, http.MethodDelete, "/api/v1/mailbox/containers/synthetic-mailbox", nil, nil)
	require.Equal(t, 204, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/mailbox/jobs?limit=101", nil)
	require.Equal(t, 422, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/mailbox/jobs/synthetic/occurrences?after=invalid", nil)
	require.Equal(t, 422, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPut, "/api/v1/mailbox/containers/synthetic/chunks/invalid", nil, nil)
	require.Equal(t, 422, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/mailbox/containers", nil, map[string]string{"unexpected": "field"})
	require.Equal(t, 422, response.StatusCode, body)
}

func TestMailboxOpenAPIHasRequestsAndStreamingStatus(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	create := doc.Paths["/api/v1/mailbox/containers"].Post
	require.NotNil(t, create.RequestBody)
	require.NotNil(t, create.Responses["201"])
	chunk := doc.Paths["/api/v1/mailbox/containers/{id}/chunks/{index}"].Put
	require.NotNil(t, chunk.RequestBody.Content["application/octet-stream"])
	require.Len(t, chunk.Parameters, 4)
	events := doc.Paths["/api/v1/mailbox/jobs/{id}/events"]
	require.NotNil(t, events)
	require.NotNil(t, events.Get.Responses["200"].Content["application/x-ndjson"])
}

func TestMailboxInternalFailureIsServerError(t *testing.T) {
	ts, s := newTestServer(t, nil)
	require.NoError(t, s.Close())
	response, body := get(t, ts, "/api/v1/mailbox/jobs", nil)
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, body)
}

func TestMailboxRequestSchemasAcceptServerDefaults(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	for path, payload := range map[string]string{
		"/containers":       `{"id":"synthetic","sha256":"` + strings.Repeat("a", 64) + `","size":3,"format":"mbox"}`,
		"/archives":         `{"id":"synthetic","description":"Synthetic mailbox"}`,
		"/jobs":             `{"id":"synthetic","container_id":"synthetic","container_sha256":"` + strings.Repeat("a", 64) + `","settings":{"destination_id":1}}`,
		"/jobs/{id}/resume": `{"request":{"id":"synthetic","container_id":"synthetic","container_sha256":"` + strings.Repeat("a", 64) + `","settings":{"destination_id":1}},"continuation":false}`,
	} {
		t.Run(path, func(t *testing.T) {
			var value any
			require.NoError(t, json.Unmarshal([]byte(payload), &value))
			schema := doc.Paths["/api/v1/mailbox"+path].Post.RequestBody.Content["application/json"].Schema
			result := &huma.ValidateResult{}
			huma.Validate(doc.Components.Schemas, schema, huma.NewPathBuffer(nil, 0), huma.ModeWriteToServer, value, result)
			require.Empty(t, result.Errors)
		})
	}
}
