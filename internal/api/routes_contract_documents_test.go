package api_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
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
	headers := map[string]string{"If-Match": `"1"`}
	resp, body = do(t, ts, http.MethodPut, path+"&content_version_id=", headers, nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPut, fmt.Sprintf("/api/v1/contracts/%s/documents/%d?role=invalid", c.ContractID, n.ID), headers, nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPut, fmt.Sprintf("/api/v1/contracts/%s/documents/%d?role=signed", c.ContractID, n.ID+10000), headers, nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPut, path, headers, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	require.Len(t, c.Documents, 1)
	old := c.Documents[0].ContentVersionID
	headers["If-Match"] = `"2"`
	resp, body = do(t, ts, http.MethodDelete, path+"&content_version_id=", headers, nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	hash, size, err := s.Blobs.WriteContext(t.Context(), strings.NewReader("replacement"))
	require.NoError(t, err)
	n, _, err = s.ReplaceContent(t.Context(), n.ID, n.Revision, hash, size, "text/plain")
	require.NoError(t, err)
	resp, body = do(t, ts, http.MethodPut, path, headers, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	headers["If-Match"] = `"3"`
	resp, body = do(t, ts, http.MethodDelete, path, headers, nil)
	require.Equal(t, http.StatusConflict, resp.StatusCode, body)
	require.Contains(t, body, "ambiguous_contract_document")
	n, _, err = s.Trash(t.Context(), n.ID, n.Revision)
	require.NoError(t, err)
	resp, body = do(t, ts, http.MethodPut, fmt.Sprintf("/api/v1/contracts/%s/documents/%d?role=draft", c.ContractID, n.ID), headers, nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	_, err = s.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	resp, body = get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/contracts", n.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.ContractPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	require.Len(t, page.Items[0].Documents, 2)
	resp, body = do(t, ts, http.MethodDelete, path+"&content_version_id="+old, headers, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	require.Len(t, c.Documents, 1)
	headers["If-Match"] = `"4"`
	resp, body = do(t, ts, http.MethodDelete, path, headers, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	require.Empty(t, c.Documents)
}
