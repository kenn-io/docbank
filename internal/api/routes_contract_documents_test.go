package api_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestContractDocumentRoutes(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	n := createFileWithContent(t, ts, s, "/contract.txt", "synthetic agreement")
	resp, body := do(t, ts, http.MethodPost, "/api/v1/contracts", nil, map[string]any{"title": "Agreement"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var c api.Contract
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	path := fmt.Sprintf("/api/v1/contracts/%s/documents/%d?role=signed", c.ContractID, n.ID)
	resp, body = do(t, ts, http.MethodPut, path, nil, nil)
	require.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPut, path, map[string]string{"If-Match": `"1"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	require.Len(t, c.Documents, 1)
	require.True(t, c.Documents[0].Current)
	require.True(t, c.Documents[0].Available)
	resp, body = get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/contracts", n.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.ContractPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	require.Len(t, page.Items[0].Documents, 1)
	resp, body = do(t, ts, http.MethodDelete, path, map[string]string{"If-Match": `"1"`}, nil)
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete, path, map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	require.Empty(t, c.Documents)
}
