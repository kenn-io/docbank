package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
)

const renderTestVault = "00000000-0000-4000-8000-000000000056"

func unnumberedTestArguments(f reviewedBatesFixture, destination string) map[string]any {
	return map[string]any{
		"vault_id": renderTestVault, "preparation_path": f.paths["preparation_path"],
		"masks_path": f.paths["masks_path"], "review_path": f.paths["review_path"], "destination_path": destination,
	}
}

func TestMCPRenderNativePDFAdmission(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	const privateSentinel = "private-render-input-value"
	for _, tc := range []struct {
		name, writer, replyCode, message string
		renderCalls                      int32
	}{
		{name: "read-only"},
		{name: "malformed review"},
		{name: "changed review", replyCode: "redaction_review_changed", message: "no longer matches"},
		{name: "old writer", writer: "docbank-fresh-objects/v2", replyCode: "redaction_review_changed", message: "older writer"},
		{name: "wrong vault", replyCode: "redaction_selection_changed", message: "vault"},
		{name: "busy", replyCode: "redaction_busy", message: "daemon", renderCalls: 1},
		{name: "memory", replyCode: "redaction_memory", message: "daemon", renderCalls: 1},
		{name: "worker timeout", replyCode: "redaction_worker_timeout", message: "daemon PDF worker", renderCalls: 1},
		{name: "archive too large", replyCode: "redaction_integrity", message: "did not match", renderCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := tc.writer
			if writer == "" {
				writer = pdfproduction.QualifiedRecipe().WriterVersion
			}
			f := reviewedBatesInputs(t, writer)
			switch tc.name {
			case "malformed review":
				require.NoError(t, os.WriteFile(f.paths["review_path"], []byte(`{"`+privateSentinel+`":true}`), 0600))
			case "changed review":
				path := f.paths["masks_path"]
				var masks []processing.RedactionMask
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(data, &masks))
				masks[0].ID = uuid.New()
				data, err = json.Marshal(masks)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0600))
			}

			var requests, renders atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/api/v1/vault":
					vault := renderTestVault
					if tc.name == "wrong vault" {
						vault = testBatesNamespaceID
					}
					writeDaemonJSON(t, w, api.VaultIdentity{VaultID: vault})
				case "/api/v1/redaction/render":
					renders.Add(1)
					assert.Equal(t, http.MethodPost, r.Method)
					var request processing.RedactionRenderRequest
					assert.NoError(t, json.UnmarshalRead(r.Body, &request))
					assert.Equal(t, f.review.SHA256, request.ReviewSHA256)
					assert.Equal(t, uuid.Nil(), request.AllocationID)
					if tc.name == "archive too large" {
						w.Header().Set("Content-Type", "application/zip")
						w.Header().Set("Content-Length", "570425345") // 544 MiB plus one.
						w.Header().Set("X-Content-Sha256", strings.Repeat("a", 64))
						w.Header().Set("X-Redaction-Review-Sha256", f.review.SHA256)
						w.WriteHeader(http.StatusOK)
						return
					}
					problem := api.NewError(http.StatusServiceUnavailable, tc.replyCode, privateSentinel)
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(problem.Status)
					assert.NoError(t, json.MarshalWrite(w, problem))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer daemon.Close()
			dir := t.TempDir()
			destination := filepath.Join(dir, "delivery.zip")
			var logs bytes.Buffer
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{
				AllowPackageWrites: tc.name != "read-only", Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
			}, exportTestLease(t, daemon.URL))
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": "render_native_pdf", "arguments": unnumberedTestArguments(f, destination),
			}))
			require.NotContains(t, string(raw)+logs.String(), privateSentinel)
			require.Equal(t, tc.renderCalls, renders.Load())
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, entries)
			if tc.replyCode == "" {
				require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
			} else {
				result := objectField(t, decodeResult(t, raw), "structuredContent")
				require.Equal(t, tc.replyCode, result["code"])
				require.Contains(t, result["message"], tc.message)
			}
			if tc.renderCalls == 0 && tc.name != "wrong vault" {
				require.Zero(t, requests.Load())
			}
		})
	}
}

func TestMCPRenderNativePDFSharesFullCheckSlot(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	f := reviewedBatesInputs(t, pdfproduction.QualifiedRecipe().WriterVersion)
	f.bindPDF(t, []byte("recorded PDF"))
	cleanup := cleanupRedactionRenderStage
	t.Cleanup(func() { cleanupRedactionRenderStage = cleanup })
	var cleaned atomic.Int32
	cleanupRedactionRenderStage = func(stage *filepublish.Stage) error {
		cleaned.Add(1)
		return errors.Join(cleanup(stage), errors.New("synthetic cleanup failure"))
	}
	entered, done := make(chan struct{}), make(chan error, 1)
	var renders atomic.Int32
	serve := reviewedBatesHandler(t, f, nil)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/vault":
			writeDaemonJSON(t, w, api.VaultIdentity{VaultID: renderTestVault})
		case "/api/v1/redaction/render":
			renders.Add(1)
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Length", "1024")
			w.Header().Set("X-Content-Sha256", strings.Repeat("a", 64))
			w.Header().Set("X-Redaction-Review-Sha256", f.review.SHA256)
			_, _ = w.Write([]byte("P"))
			_ = http.NewResponseController(w).Flush()
			close(entered)
			<-r.Context().Done()
		default:
			serve(w, r)
		}
	}))
	defer daemon.Close()
	dir := t.TempDir()
	lease := exportTestLease(t, daemon.URL)
	destination := filepath.Join(dir, "delivery.zip")
	args := unnumberedTestArguments(f, destination)
	encoded, err := json.Marshal(args)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _, err := renderNativePDF(ctx, lease, encoded, slog.Default()); done <- err }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("render ended before transfer: %v", err)
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPackageWrites: true}, lease)
	peer := recipientPackageArguments(t, f)
	peer["selection_path"] = filepath.Join(t.TempDir(), "absent.json")
	require.Equal(t, "redaction_busy", exportCall(t, server, "export_recipient_package", peer)["code"])
	require.Equal(t, "redaction_busy", exportCall(t, server, "export_bates_file", f.arguments)["code"])
	read := exportCall(t, server, "get_bates_export", map[string]any{"allocation_id": f.receipt.AllocationID})
	require.Equal(t, f.receipt.BlobSHA256, read["blob_sha256"])
	cancel()
	code, _ := stableDomainError(<-done)
	require.Equal(t, "redaction_canceled", code)
	require.EqualValues(t, 1, renders.Load())
	require.EqualValues(t, 1, cleaned.Load())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
	raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_recipient_package", "arguments": peer}))
	require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
}

type interruptedPDFReader struct {
	ctx     context.Context
	entered chan struct{}
}

func (r interruptedPDFReader) ReadAt([]byte, int64) (int, error) {
	close(r.entered)
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func TestMCPRenderNativePDFLocalVerificationCancellation(t *testing.T) {
	f := reviewedBatesInputs(t, pdfproduction.QualifiedRecipe().WriterVersion)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := interruptedPDFReader{ctx: ctx, entered: make(chan struct{})}
	contents := daemonconn.RedactionArchiveContents{PDF: io.NewSectionReader(reader, 0, 128)}
	done := make(chan error, 1)
	go func() { done <- checkRenderedNativePDF(ctx, contents, f.review) }()
	select {
	case <-reader.entered:
	case err := <-done:
		t.Fatalf("verification ended before reading the PDF: %v", err)
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	cancel()
	code, _ := stableDomainError(<-done)
	require.Equal(t, "redaction_canceled", code)
}

func TestMCPRenderNativePDFLocalVerificationRefusals(t *testing.T) {
	for _, tc := range []struct {
		err           error
		code, message string
	}{
		{&pdfproduction.RSSLimitError{LimitBytes: 1}, "redaction_memory", "The MCP process"},
		{context.DeadlineExceeded, "redaction_worker_timeout", "The MCP PDF verifier"},
	} {
		var failure *renderError
		require.ErrorAs(t, renderVerificationError(t.Context(), tc.err), &failure)
		require.Equal(t, tc.code, failure.code)
		require.Contains(t, failure.message, tc.message)
	}
}
