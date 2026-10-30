package api_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
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
	ts, vault := newTestServer(t, func(d *api.Deps) {
		service, err := processing.NewService(processing.ServiceConfig{Catalog: d.Store, Blobs: d.Blobs, Gate: api.NewOperationGate(), SpoolDirectory: filepath.Join(d.VaultRoot, "blobs", "tmp")})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, service.Shutdown(context.Background())) })
		d.Processing, d.PageRuntime = service, runtime
		processingService = service
	})
	pdf := redactiontest.PDF(t, []string{"SECRET KEEP SECRET"}, "")
	hash, size, err := vault.Blobs.Write(bytes.NewReader(pdf))
	require.NoError(t, err)
	node, err := vault.CreateFile(t.Context(), vault.RootID(), "synthetic.pdf", hash, size, "application/pdf")
	require.NoError(t, err)
	issue := func() string {
		resp, err := ts.Client().Post(ts.URL+"/api/daemon/web-session", "application/json", nil)
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
		req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(data))
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
		resp, err := http.Get(ts.URL + ticket.URL)
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
		resp, err = http.Get(ts.URL + ticket.URL)
		require.NoError(t, err)
		require.Equal(t, 404, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	}
	resp, body = call(alice, "DELETE", base, nil)
	require.Equal(t, 204, resp.StatusCode, string(body))
	resp, body = call(alice, "DELETE", base, nil)
	require.Equal(t, 410, resp.StatusCode, string(body))
	// Recreate a preview and leave a real ticket transfer blocked on a small
	// TCP receive window. Shutdown must interrupt its socket write and release
	// the directory, rather than waiting for the delivery deadline.
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
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	tcp, ok := conn.(*net.TCPConn)
	require.True(t, ok)
	require.NoError(t, tcp.SetReadBuffer(1024))
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", ticket.URL, strings.TrimPrefix(ts.URL, "http://"))
	require.NoError(t, err)
	blocked, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, 200, blocked.StatusCode)
	shutdown, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, processingService.Shutdown(shutdown), "non-reading browser held preview through shutdown")
}
