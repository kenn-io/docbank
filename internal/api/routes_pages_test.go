package api_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPageAPIRequiresExactSelectionAndConfiguredRuntime(t *testing.T) {
	ts, s := newTestServer(t, nil)
	data := []byte("synthetic unsupported source")
	hash, size, err := s.Blobs.Write(bytes.NewReader(data))
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.pdf", hash, size, "application/pdf")
	require.NoError(t, err)
	binding := store.PageBinding{NodeID: node.ID, Revision: node.Revision, Source: document.PageSource{VersionID: node.CurrentVersionID, SHA256: hash, Size: size}}
	response, body := do(t, ts, http.MethodPost, "/api/v1/pages/inventory", nil, api.PageSelectionRequest{Selection: binding})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/pages/jobs", nil, api.PageRenderRequest{OperationID: uuid.NewString(), Selection: binding, Pages: []int{1}, DPI: 144})
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode, body)
	binding.Revision++
	response, body = do(t, ts, http.MethodPost, "/api/v1/pages/inventory", nil, api.PageSelectionRequest{Selection: binding})
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
}
