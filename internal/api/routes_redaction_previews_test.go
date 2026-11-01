package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionPreviewBrowserHTTP(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	var processingService *processing.Service
	var spool string
	ts, vault := newTestServer(t, func(d *api.Deps) {
		spool = filepath.Join(d.VaultRoot, "blobs", "tmp")
		service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: d.Store, Blobs: d.Blobs, Gate: api.NewOperationGate(), SpoolDirectory: spool})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, service.Shutdown(context.Background())) })
		d.Processing, d.PageRuntime = service, runtime
		processingService = service
	})
	pdf := redactiontest.PDF(t, []string{"SECRET KEEP SECRET"}, "")
	hash, size, err := vault.Blobs.WriteContext(t.Context(), bytes.NewReader(pdf))
	require.NoError(t, err)
	node, err := vault.CreateFile(t.Context(), vault.RootID(), "synthetic.pdf", hash, size, "application/pdf")
	require.NoError(t, err)
	issue := func() string {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/daemon/web-session", nil)
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		resp, err := ts.Client().Do(request)
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		require.Equal(t, 201, resp.StatusCode)
		var session struct {
			Token string `json:"token"`
		}
		require.NoError(t, json.UnmarshalRead(resp.Body, &session))
		return session.Token
	}
	alice, bob := issue(), issue()
	call := func(token, method, path string, input any) (*http.Response, []byte) {
		t.Helper()
		data, err := json.Marshal(input)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+path, bytes.NewReader(data))
		require.NoError(t, err)
		req.Header.Set(api.WebSessionHeader, token)
		req.Header.Set("Content-Type", "application/json")
		req.Header["X-Api-Key"] = []string{""}
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		data, err = io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp, data
	}
	resp, body := call(alice, "POST", "/api/v1/redaction/prepare", processing.RedactionPreparationRequest{Selection: store.PageBinding{NodeID: node.ID, Revision: node.Revision, Source: document.PageSource{VersionID: node.CurrentVersionID, SHA256: hash, Size: size}}})
	require.Equal(t, 200, resp.StatusCode, string(body))
	var prep processing.RedactionPreparation
	require.NoError(t, json.Unmarshal(body, &prep))
	masks := []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{End: 6}}}}
	resp, body = call(alice, "POST", "/api/v1/redaction/resolve", processing.RedactionResolveRequest{Preparation: prep, Masks: masks})
	require.Equal(t, 200, resp.StatusCode, string(body))
	var review processing.RedactionReview
	require.NoError(t, json.Unmarshal(body, &review))
	request := map[string]any{"preparation": prep, "masks": masks, "review_sha256": review.SHA256, "allocation_id": nil}
	resp, body = call(alice, "POST", "/api/v1/redaction/previews", request)
	require.Equal(t, 400, resp.StatusCode, string(body))
	delete(request, "allocation_id")
	for _, path := range []string{"render", "reservations"} {
		resp, _ = call(alice, "POST", "/api/v1/redaction/"+path, request)
		require.Equal(t, 403, resp.StatusCode)
	}
	resp, body = call(alice, "POST", "/api/v1/redaction/previews", request)
	require.Equal(t, 201, resp.StatusCode, string(body))
	var preview processing.RedactionPreview
	require.NoError(t, json.Unmarshal(body, &preview))
	base := "/api/v1/redaction/previews/" + preview.ID
	for _, path := range []string{"", "/text", "/pages/1"} {
		resp, body = call(bob, "GET", base+path, nil)
		require.Equal(t, 410, resp.StatusCode, string(body))
	}
	resp, body = call(bob, "DELETE", base, nil)
	require.Equal(t, 410, resp.StatusCode, string(body))
	resp, body = call(bob, "DELETE", "/api/v1/redaction/previews", nil)
	require.Equal(t, 204, resp.StatusCode, string(body))
	resp, body = call(alice, "GET", base+"/text", nil)
	require.Equal(t, 200, resp.StatusCode, string(body))
	require.Equal(t, "[REDACTED] KEEP SECRET\f", string(body))
	resp, body = call(alice, "GET", base+"/pages/1", nil)
	require.Equal(t, 200, resp.StatusCode)
	digest := sha256.Sum256(body)
	require.Equal(t, preview.Pages[0].PNG.SHA256, hex.EncodeToString(digest[:]))
	resp, _ = call(alice, "GET", base+"/bundle", nil)
	require.Equal(t, 403, resp.StatusCode)
	resp, _ = call(alice, "GET", base+"?unexpected=true", nil)
	require.Equal(t, 403, resp.StatusCode)
	confirm := map[string]any{"confirmed": true, "review_sha256": review.SHA256, "archive_sha256": strings.Repeat("0", 64)}
	resp, _ = call(alice, "POST", base+"/download", confirm)
	require.Equal(t, 409, resp.StatusCode)
	confirm["archive_sha256"] = preview.Archive.SHA256
	var first []byte
	for range 2 {
		resp, body = call(alice, "POST", base+"/download", confirm)
		require.Equal(t, 200, resp.StatusCode, string(body))
		var ticket struct {
			URL string `json:"url"`
		}
		require.NoError(t, json.Unmarshal(body, &ticket))
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+ticket.URL, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode)
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		if first == nil {
			first = data
		} else {
			require.Equal(t, first, data)
		}
		digest = sha256.Sum256(data)
		require.Equal(t, preview.Archive.SHA256, hex.EncodeToString(digest[:]))
		request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+ticket.URL, nil)
		require.NoError(t, err)
		resp, err = http.DefaultClient.Do(request)
		require.NoError(t, err)
		require.Equal(t, 404, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	}
	t.Run("cleanup failure", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions")
		}
		directories, err := filepath.Glob(filepath.Join(spool, "native-pdf-*"))
		require.NoError(t, err)
		require.Len(t, directories, 1)
		directory := directories[0]
		require.NoError(t, os.Chmod(directory, 0o500))
		defer func() { require.NoError(t, os.Chmod(directory, 0o700)) }()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, ts.URL+"/api/v1/redaction/previews", nil)
		require.NoError(t, err)
		req.Header.Set(api.WebSessionHeader, alice)
		req.Header["X-Api-Key"] = []string{""}
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		var problem api.Error
		require.NoError(t, json.UnmarshalRead(resp.Body, &problem))
		require.Equal(t, "redaction_failed", problem.Code)
		files, err := os.ReadDir(directory)
		require.NoError(t, err)
		require.NotEmpty(t, files, "failed discard must retain the preview for cleanup retry")
	})
	// Restored permissions let the same owner retry cleanup and create another preview.
	resp, body = call(alice, "DELETE", "/api/v1/redaction/previews", nil)
	require.Equal(t, 204, resp.StatusCode, string(body))
	resp, body = call(alice, "GET", fmt.Sprintf("/api/v1/nodes/%d", node.ID), nil)
	require.Equal(t, 200, resp.StatusCode, "discard must preserve the browser session: %s", body)
	resp, body = call(alice, "DELETE", base, nil)
	require.Equal(t, 410, resp.StatusCode, string(body))
	// A zero-buffer connection forces backpressure even for a small archive.
	// Keep real preview acquisition and delivery; replace only the output socket.
	resp, body = call(alice, "POST", "/api/v1/redaction/previews", request)
	require.Equal(t, 201, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &preview))
	base = "/api/v1/redaction/previews/" + preview.ID
	confirm["archive_sha256"] = preview.Archive.SHA256
	resp, body = call(alice, "POST", base+"/download", confirm)
	require.Equal(t, 200, resp.StatusCode, string(body))
	var ticket struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(body, &ticket))
	server, client := net.Pipe()
	defer func() { _ = server.Close(); _ = client.Close() }()
	writing := make(chan struct{}, 1)
	output := &previewBlockedResponse{ResponseRecorder: httptest.NewRecorder(), Conn: server, writing: writing}
	finished := make(chan any, 1)
	go func() {
		defer func() { finished <- recover() }()
		ts.Config.Handler.ServeHTTP(output, httptest.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+ticket.URL, nil))
	}()
	select {
	case <-writing:
	case <-time.After(5 * time.Second):
		t.Fatal("preview delivery never reached the blocked socket")
	}
	select {
	case result := <-finished:
		t.Fatalf("delivery finished before shutdown: %v", result)
	default:
	}
	shutdown, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, processingService.Shutdown(shutdown), "non-reading browser held preview through shutdown")
	select {
	case result := <-finished:
		require.Equal(t, http.ErrAbortHandler, result)
	case <-shutdown.Done():
		t.Fatal("shutdown did not interrupt preview delivery")
	}
	require.Equal(t, http.StatusOK, output.Code)
	directories, err := filepath.Glob(filepath.Join(spool, "native-pdf-*"))
	require.NoError(t, err)
	require.Empty(t, directories, "shutdown must release the preview spool")
}

// previewBlockedResponse preserves HTTP headers but sends bytes over net.Pipe,
// whose Write cannot complete until the peer reads or its deadline interrupts it.
type previewBlockedResponse struct {
	*httptest.ResponseRecorder
	net.Conn

	writing chan struct{}
}

func (w *previewBlockedResponse) Write(data []byte) (int, error) {
	select {
	case w.writing <- struct{}{}:
	default:
	}
	n, err := w.Conn.Write(data)
	if err != nil {
		return n, fmt.Errorf("writing preview to blocked connection: %w", err)
	}
	return n, nil
}
