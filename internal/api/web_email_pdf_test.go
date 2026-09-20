package api_test

import (
	"bytes"
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"net/http"
	"strings"
	"testing"
)

func TestWebEmailPDFDownloadRequiresRetainedReceipt(t *testing.T) {
	ts, s := newTestServer(t, nil)
	node := createFileWithContent(t, ts, s, "/synthetic.eml", "Subject: Synthetic\r\n\r\nbody")
	b, err := json.Marshal(map[string]any{"node_id": node.ID, "revision": node.Revision, "version_id": node.CurrentVersionID, "blob_hash": node.BlobHash, "size": node.Size, "email_pdf_profile": strings.Repeat("a", 64), "email_pdf_attachment": strings.Repeat("b", 64)})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/daemon/web-download", bytes.NewReader(b))
	require.NoError(t, err)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
