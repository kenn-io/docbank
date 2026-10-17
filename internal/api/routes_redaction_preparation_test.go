package api_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionPreparationHTTP(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	ts, s := newTestServer(t, func(d *api.Deps) {
		service, err := processing.NewService(processing.ServiceConfig{Catalog: d.Store, Blobs: d.Blobs,
			Gate: api.NewOperationGate(), SpoolDirectory: filepath.Join(d.VaultRoot, "blobs", "tmp")})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, service.Shutdown(context.Background())) })
		d.Processing, d.PageRuntime = service, runtime
	})
	data := redactiontest.PDF(t, []string{"SECRET KEEP SECRET"}, "")
	hash, size, err := s.Blobs.Write(bytes.NewReader(data))
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), "native.pdf", hash, size, "application/pdf")
	require.NoError(t, err)
	request := processing.RedactionPreparationRequest{Selection: store.PageBinding{NodeID: node.ID,
		Revision: node.Revision, Source: document.PageSource{VersionID: node.CurrentVersionID, SHA256: hash, Size: size}}}
	const path = "/api/v1/redaction/prepare"
	response, body := do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, 200, response.StatusCode, body)
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	var result processing.RedactionPreparation
	require.NoError(t, json.Unmarshal([]byte(body), &result))
	require.NoError(t, processing.ValidateRedactionPreparation(result))
	require.Equal(t, "SECRET KEEP SECRET", result.Map.Text)
	require.Equal(t, request.Selection, result.Selection)
	response, body = do(t, ts, http.MethodPost, path,
		map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}, request)
	require.Equal(t, 403, response.StatusCode, body)
	require.Contains(t, body, "web_session_read_only")
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	var malformed map[string]any
	require.NoError(t, json.Unmarshal(encoded, &malformed))
	selection, ok := malformed["selection"].(map[string]any)
	require.True(t, ok)
	selection["extra"] = true
	response, body = do(t, ts, http.MethodPost, path, nil, malformed)
	require.Equal(t, 422, response.StatusCode, body)
	require.Contains(t, body, "validation")

	client := daemonconn.New(ts.URL, testAPIKey)
	defer func() { require.NoError(t, client.Close()) }()
	received, err := client.PrepareNativePDF(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, result.Map, received.Map)
	for _, sample := range []struct {
		name, content, cmap string
		unordered           bool
	}{
		{"inked-alias", "BT /F1 24 Tf 72 720 Td (A) Tj ET",
			"1 begincodespacerange <00> <FF> endcodespacerange 1 beginbfchar <41> <0020> endbfchar", false},
		{"painted", "BT /F1 12 Tf 72 720 Td (KEEP) Tj ET 72 600 40 40 re f", "", true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			pdf := redactiontest.FontPDF(sample.content,
				"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding /ToUnicode 6 0 R >>", sample.cmap)
			hash, size, err := s.Blobs.Write(bytes.NewReader(pdf))
			require.NoError(t, err)
			node, err := s.CreateFile(t.Context(), s.RootID(), sample.name+".pdf", hash, size, "application/pdf")
			require.NoError(t, err)
			req := processing.RedactionPreparationRequest{Selection: store.PageBinding{NodeID: node.ID,
				Revision: node.Revision, Source: document.PageSource{VersionID: node.CurrentVersionID, SHA256: hash, Size: size}}}
			prepared, err := client.PrepareNativePDF(t.Context(), req)
			require.NoError(t, err)
			require.NotEmpty(t, prepared.Map.Gaps)
			for _, gap := range prepared.Map.Gaps {
				require.Equal(t, sample.unordered, gap.Unordered)
			}
			_, err = redaction.Resolve(prepared.Map, "redact_selected", nil, pdfproduction.QualifiedRecipe())
			require.ErrorContains(t, err, "mapping_incomplete")
		})
	}
	request.Selection.Revision++
	response, body = do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, 409, response.StatusCode, body)
	require.Contains(t, body, "redaction_selection_changed")
}
