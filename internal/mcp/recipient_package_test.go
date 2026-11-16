package mcp

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/recipientpackage"
)

func recipientPackageArguments(t *testing.T, f reviewedBatesFixture) map[string]any {
	t.Helper()
	selection := recipientpackage.Selection{Contract: "native-pdf-recipient-selection/v1", Documents: []recipientpackage.Input{{
		AllocationID: f.receipt.AllocationID, Preparation: f.paths["preparation_path"], Masks: f.paths["masks_path"], Review: f.paths["review_path"],
	}}}
	data, err := json.Marshal(selection)
	require.NoError(t, err)
	selectionPath := filepath.Join(t.TempDir(), "selection.json")
	require.NoError(t, os.WriteFile(selectionPath, data, 0600))
	return map[string]any{"selection_path": selectionPath, "destination_path": filepath.Join(t.TempDir(), "recipient.zip")}
}

func TestRecipientPackageOptIn(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	f.bindPDF(t, []byte("not downloaded"))
	var requests atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		writeDaemonJSON(t, w, f.receipt)
	}))
	defer daemon.Close()
	server := newBatesToolTestServer(t, daemon.URL, false)
	raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_recipient_package", "arguments": recipientPackageArguments(t, f)}))
	require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	require.Zero(t, requests.Load())
	require.NotContains(t, catalogNames(toolCatalog(ServerOptions{})), "export_recipient_package")
	require.Contains(t, catalogNames(toolCatalog(ServerOptions{AllowPackageWrites: true})), "export_recipient_package")
}

func TestRecipientPackageExport(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	for _, writer := range []string{"docbank-fresh-objects/v2", "docbank-fresh-objects/v3"} {
		t.Run(writer, func(t *testing.T) {
			f := reviewedBatesInputs(t, writer)
			data := f.pdf(t, false)
			f.bindPDF(t, data)
			daemon := httptest.NewServer(reviewedBatesHandler(t, f, data))
			defer daemon.Close()
			args := recipientPackageArguments(t, f)
			destination, ok := args["destination_path"].(string)
			require.True(t, ok)
			server := newBatesToolTestServer(t, daemon.URL, true)
			transport := "stdio"
			if writer == "docbank-fresh-objects/v3" {
				transport = "http"
			}
			raw := exportExchange(t, server, transport, "tools/call", map[string]any{"name": "export_recipient_package", "arguments": args})
			result := objectField(t, decodeResult(t, raw), "structuredContent")
			require.Equal(t, "published", result["state"], string(raw))
			require.Equal(t, "masks_text_and_numbers", result["verification"])
			require.Equal(t, false, result["cleanup_failed"])
			saved, err := os.ReadFile(destination)
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(saved)), result["sha256"])
			archive, err := zip.NewReader(bytes.NewReader(saved), int64(len(saved)))
			require.NoError(t, err)
			receiptFile, err := archive.Open("receipt.json")
			require.NoError(t, err)
			receiptBytes, err := io.ReadAll(receiptFile)
			require.NoError(t, err)
			require.NoError(t, receiptFile.Close())
			require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(receiptBytes)), result["receipt_sha256"])
			var receipt processing.RecipientPackageReceipt
			encoded, err := json.Marshal(result["receipt"])
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(encoded, &receipt))
			require.Equal(t, int64(23), receipt.Documents[0].Text.Size)
			require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("[REDACTED] KEEP SECRET\f"))), receipt.Documents[0].Text.SHA256)
			require.NoError(t, processing.VerifyRecipientPackage(t.Context(), bytes.NewReader(saved), int64(len(saved)), receipt))
			again := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_recipient_package", "arguments": args}))
			require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, again).Code)
			unchanged, err := os.ReadFile(destination)
			require.NoError(t, err)
			require.Equal(t, saved, unchanged)
		})
	}
}

func TestRecipientPackagePrivateInputAndStampRefusals(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	for _, mode := range []string{"private review", "missing selection", "ordinary stamp", "partial", "null", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
			f.bindPDF(t, []byte("not downloaded"))
			if mode == "ordinary stamp" {
				_, f.receipt = syntheticStampedBatesExport(t)
			}
			args := recipientPackageArguments(t, f)
			destination, ok := args["destination_path"].(string)
			require.True(t, ok)
			switch mode {
			case "private review":
				require.NoError(t, os.WriteFile(f.paths["review_path"], []byte(`{"recipient-private-sentinel":true}`), 0600))
			case "missing selection":
				args["selection_path"] = filepath.Join(t.TempDir(), "recipient-private-sentinel.json")
			case "partial":
				delete(args, "selection_path")
			case "null":
				args["selection_path"] = nil
			case "unknown":
				args["recipient-private-sentinel"] = true
			}
			var requests, downloads atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if strings.HasSuffix(r.URL.Path, "/content") {
					downloads.Add(1)
				}
				writeDaemonJSON(t, w, f.receipt)
			}))
			defer daemon.Close()
			var logs bytes.Buffer
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPackageWrites: true, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}, exportTestLease(t, daemon.URL))
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_recipient_package", "arguments": args}))
			require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
			require.Zero(t, downloads.Load())
			if mode != "ordinary stamp" && mode != "private review" {
				require.Zero(t, requests.Load())
			}
			require.NotContains(t, string(raw)+logs.String(), "recipient-private-sentinel")
			require.NoFileExists(t, destination)
		})
	}
}

func TestRecipientPackageGateAndCancellation(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	data := []byte("held transfer")
	f.bindPDF(t, data)
	entered, canceled := make(chan struct{}), make(chan struct{})
	var downloads atomic.Int32
	serve := reviewedBatesHandler(t, f, data)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/content") {
			downloads.Add(1)
			_, _ = w.Write(data[:1])
			_ = http.NewResponseController(w).Flush()
			close(entered)
			<-r.Context().Done()
			close(canceled)
			return
		}
		serve(w, r)
	}))
	defer daemon.Close()
	lease := exportTestLease(t, daemon.URL)
	args := recipientPackageArguments(t, f)
	destination, ok := args["destination_path"].(string)
	require.True(t, ok)
	encoded, err := json.Marshal(args)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := exportRecipientPackage(ctx, lease, encoded, slog.Default()); done <- err }()
	<-entered
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPackageWrites: true}, lease)
	peer := recipientPackageArguments(t, f)
	peer["selection_path"] = filepath.Join(t.TempDir(), "absent.json")
	result := exportCall(t, server, "export_recipient_package", peer)
	require.Equal(t, "redaction_busy", result["code"])
	result = exportCall(t, server, "export_bates_file", f.arguments)
	require.Equal(t, "redaction_busy", result["code"])
	read := exportCall(t, server, "get_bates_export", map[string]any{"allocation_id": f.receipt.AllocationID})
	require.Equal(t, f.receipt.BlobSHA256, read["blob_sha256"])
	require.EqualValues(t, 1, downloads.Load())
	cancel()
	code, _ := stableDomainError(<-done)
	require.Equal(t, "redaction_canceled", code)
	<-canceled
	entries, err := os.ReadDir(filepath.Dir(destination))
	require.NoError(t, err)
	require.Empty(t, entries)
	raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_recipient_package", "arguments": peer}))
	require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
}

func TestRecipientPackageCleanupAndIntegrity(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	for _, mode := range []string{"cleanup", "publication conflict", "changed pixels"} {
		t.Run(mode, func(t *testing.T) {
			f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
			data := f.pdf(t, mode == "changed pixels")
			f.bindPDF(t, data)
			args := recipientPackageArguments(t, f)
			destination, ok := args["destination_path"].(string)
			require.True(t, ok)
			clean := cleanupRecipientStage
			t.Cleanup(func() { cleanupRecipientStage = clean })
			if mode == "cleanup" {
				cleanupRecipientStage = func(stage *filepublish.Stage) error {
					require.True(t, reviewedBatesActive.Load(), "capacity opens after cleanup")
					require.NoError(t, clean(stage))
					return errors.New("injected stage removal failure")
				}
			}
			serve := reviewedBatesHandler(t, f, data)
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "publication conflict" && strings.HasSuffix(r.URL.Path, "/content") {
					assert.NoError(t, os.WriteFile(destination, []byte("concurrent file"), 0600))
				}
				serve(w, r)
			}))
			defer daemon.Close()
			var logs bytes.Buffer
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPackageWrites: true, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}, exportTestLease(t, daemon.URL))
			result := exportCall(t, server, "export_recipient_package", args)
			switch mode {
			case "cleanup":
				require.Equal(t, "published", result["state"])
				require.Equal(t, true, result["cleanup_failed"])
				require.FileExists(t, destination)
				cleanupRecipientStage = clean
			case "publication conflict":
				require.Equal(t, recipientPackageLocalIOCode, result["code"])
				saved, err := os.ReadFile(destination)
				require.NoError(t, err)
				require.Equal(t, "concurrent file", string(saved))
				var event map[string]any
				require.NoError(t, json.UnmarshalRead(&logs, &event))
				require.Contains(t, event["error"], destination)
			case "changed pixels":
				require.Equal(t, "redaction_integrity", result["code"])
				require.NoFileExists(t, destination)
			}
			args["destination_path"] = filepath.Join(t.TempDir(), "retry.zip")
			args["selection_path"] = filepath.Join(t.TempDir(), "missing.json")
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_recipient_package", "arguments": args}))
			require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
		})
	}
}

func TestRecipientPackageHTTPDeadline(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	data := []byte("held transfer")
	f.bindPDF(t, data)
	args := recipientPackageArguments(t, f)
	destination, ok := args["destination_path"].(string)
	require.True(t, ok)
	synctest.Test(t, func(t *testing.T) {
		serve := reviewedBatesHandler(t, f, data)
		canceled := make(chan struct{})
		upstream := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/content") {
				_, _ = w.Write(data[:1])
				_ = http.NewResponseController(w).Flush()
				<-r.Context().Done()
				close(canceled)
				return
			}
			serve(w, r)
		}))
		previous := http.DefaultTransport
		http.DefaultTransport = upstream.Client().Transport
		t.Cleanup(func() { http.DefaultTransport = previous })
		server := newBatesToolTestServer(t, upstream.URL, true)
		handler, err := server.HTTPTransportHandler(HTTPOptions{BearerToken: testMCPBearer, limits: httpLimits{RequestTimeout: time.Minute}})
		require.NoError(t, err)
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1/mcp", bytes.NewReader(requestFor("tools/call", map[string]any{"name": "export_recipient_package", "arguments": args})))
		request.Header = protocolHeaders("tools/call", "export_recipient_package")
		request.Header.Set("Authorization", "Bearer "+testMCPBearer)
		response := httptest.NewRecorder()
		start := time.Now()
		handler.ServeHTTP(response, request)
		require.Equal(t, time.Minute, time.Since(start))
		<-canceled
		synctest.Wait()
		require.NoFileExists(t, destination)
		args["selection_path"] = filepath.Join(t.TempDir(), "missing.json")
		raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_recipient_package", "arguments": args}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	})
}

func TestRecipientPackageTypedLimit(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	args := recipientPackageArguments(t, f)
	selectionPath, ok := args["selection_path"].(string)
	require.True(t, ok)
	data, err := json.Marshal(recipientpackage.Selection{Contract: "native-pdf-recipient-selection/v1", Documents: make([]recipientpackage.Input, 17)})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(selectionPath, data, 0600))
	var requests atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer daemon.Close()
	server := newBatesToolTestServer(t, daemon.URL, true)
	result := exportCall(t, server, "export_recipient_package", args)
	require.Equal(t, recipientPackageLimitCode, result["code"])
	require.Zero(t, requests.Load())
}
