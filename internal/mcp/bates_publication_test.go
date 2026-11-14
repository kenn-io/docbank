package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func publicationArguments(f reviewedBatesFixture) map[string]any {
	return map[string]any{"allocation_id": testBatesAllocationID, "review_sha256": f.review.SHA256,
		"preparation_path": f.paths["preparation_path"], "masks_path": f.paths["masks_path"],
		"review_path": f.paths["review_path"]}
}

func TestPublishNumberedPDFRefusesUnboundReplies(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	f.bindPDF(t, []byte("recorded PDF identity"))
	other := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	other.bindPDF(t, []byte("other recorded PDF"))
	for _, scenario := range []string{"other review", "allocation", "ordinary stamp", "oversized", "unknown member", "truncated"} {
		t.Run(scenario, func(t *testing.T) {
			receipt := f.receipt
			switch scenario {
			case "other review":
				receipt = other.receipt
			case "allocation":
				changed := f
				changed.receipt.Allocation = new(*f.receipt.Allocation)
				id := uuid.New().String()
				changed.receipt.ArtifactID, changed.receipt.AllocationID = id, id
				changed.receipt.Allocation.AllocationID = id
				changed.bindPDF(t, []byte("recorded PDF identity"))
				receipt = changed.receipt
			case "ordinary stamp":
				changed := f
				changed.receipt.Allocation = new(*f.receipt.Allocation)
				stamp := syntheticBatesRecipe()
				encoded, err := canonical.Marshal(stamp)
				require.NoError(t, err)
				digest, err := stamp.SHA256()
				require.NoError(t, err)
				request, err := f.numbered.PlanRequest(uuid.MustParse(testBatesSnapshotID))
				require.NoError(t, err)
				request.RecipeSHA256 = digest
				requestDigest, err := store.BatesRequestDigest(request)
				require.NoError(t, err)
				changed.receipt.Recipe, changed.receipt.RecipeSHA256 = encoded, digest
				changed.receipt.Allocation.RecipeSHA256 = digest
				changed.receipt.Allocation.RequestSHA256 = requestDigest
				changed.bindPDF(t, []byte("ordinary stamped PDF identity"))
				receipt = changed.receipt
			}
			data, err := json.Marshal(receipt)
			require.NoError(t, err)
			switch scenario {
			case "unknown member":
				data = append([]byte(`{"private-marker":"publication-private-value",`), data[1:]...)
			case "truncated":
				data = data[:len(data)/2]
			}
			var posts atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				posts.Add(1)
				_, _ = w.Write(data)
				if scenario == "oversized" {
					_, _ = io.WriteString(w, strings.Repeat(" ", 128<<10))
				}
			}))
			defer daemon.Close()
			var logs bytes.Buffer
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPackageWrites: true,
				Logger: slog.New(slog.NewJSONHandler(&logs, nil))}, exportTestLease(t, daemon.URL))
			result := callToolResult(t, server, "publish_numbered_pdf", publicationArguments(f))
			require.Equal(t, true, result["isError"])
			content := objectField(t, result, "structuredContent")
			require.Equal(t, "bates_outcome_unknown", content["code"])
			require.NotContains(t, content, "artifact_id")
			require.NotContains(t, logs.String(), "publication-private-value")
			require.Equal(t, int32(1), posts.Load())
		})
	}
}

func TestPublishNumberedPDFDaemonRefusals(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	for _, code := range []string{"redaction_busy", "redaction_memory", "redaction_verifier_version",
		"redaction_review_changed", "redaction_preparation_changed", "redaction_worker_timeout", "not_found",
		"redaction_integrity", "redaction_canceled", "redaction_timeout", "invalid_redaction_request", "invalid_redaction_preparation"} {
		t.Run(code, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				status := http.StatusUnprocessableEntity
				switch code {
				case "redaction_busy", "redaction_memory", "redaction_worker_timeout":
					status = http.StatusServiceUnavailable
				case "redaction_verifier_version", "redaction_review_changed", "redaction_preparation_changed":
					status = http.StatusConflict
				case "not_found":
					status = http.StatusNotFound
				case "redaction_integrity":
					status = http.StatusInternalServerError
				case "redaction_canceled":
					status = http.StatusRequestTimeout
				case "redaction_timeout":
					status = http.StatusGatewayTimeout
				}
				w.WriteHeader(status)
				writeDaemonJSON(t, w, api.Error{Status: status, Code: code, Detail: "publication-private-value"})
			}))
			defer daemon.Close()
			server := newBatesToolTestServer(t, daemon.URL, true)
			wire := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": "publish_numbered_pdf", "arguments": publicationArguments(f),
			}))
			if strings.HasPrefix(code, "invalid_") {
				require.Equal(t, int64(-32602), decodeWireError(t, wire).Code)
				return
			}
			content := objectField(t, decodeResult(t, wire), "structuredContent")
			want := code
			if code == "redaction_integrity" || code == "redaction_canceled" || code == "redaction_timeout" {
				want = "bates_outcome_unknown"
			}
			require.Equal(t, want, content["code"])
			require.NotContains(t, string(wire), "publication-private-value")
			if code == "redaction_busy" || code == "redaction_memory" {
				require.Contains(t, content["message"], "daemon")
			}
		})
	}
}

func TestPublishNumberedPDFLostReply(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	f.bindPDF(t, []byte("recorded PDF identity"))
	var posts atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if posts.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !assert.True(t, ok) {
				return
			}
			connection, _, err := hijacker.Hijack()
			if assert.NoError(t, err) {
				assert.NoError(t, connection.Close())
			}
			return
		}
		writeDaemonJSON(t, w, f.receipt)
	}))
	defer daemon.Close()
	server := newBatesToolTestServer(t, daemon.URL, true)
	first := callToolResult(t, server, "publish_numbered_pdf", publicationArguments(f))
	content := objectField(t, first, "structuredContent")
	require.Equal(t, "bates_outcome_unknown", content["code"])
	require.Contains(t, content["message"], "identical allocation")
	require.Equal(t, int32(1), posts.Load())
	second := callToolResult(t, server, "publish_numbered_pdf", publicationArguments(f))
	require.Equal(t, f.receipt.BlobSHA256, objectField(t, second, "structuredContent")["blob_sha256"])
	require.Equal(t, int32(2), posts.Load())
}

func TestPublishNumberedPDFPrivateInputs(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	for _, scenario := range []string{"unknown member", "missing file"} {
		t.Run(scenario, func(t *testing.T) {
			f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
			f.paths["review_path"] = filepath.Join(t.TempDir(), "publication-private-value.json")
			if scenario == "unknown member" {
				require.NoError(t, os.WriteFile(f.paths["review_path"], []byte(`{"publication-private-value":true}`), 0600))
			}
			var logs bytes.Buffer
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPackageWrites: true,
				Logger: slog.New(slog.NewJSONHandler(&logs, nil))}, exportTestLease(t, "http://127.0.0.1:1"))
			wire := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": "publish_numbered_pdf", "arguments": publicationArguments(f),
			}))
			require.Equal(t, int64(-32602), decodeWireError(t, wire).Code)
			require.NotContains(t, string(wire)+logs.String(), "publication-private-value")
		})
	}
}

func TestPublishNumberedPDFConcurrencyAndCancellation(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	f.bindPDF(t, []byte("recorded PDF identity"))
	entered, canceled := make(chan struct{}), make(chan struct{})
	var posts atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeDaemonJSON(t, w, f.receipt)
			return
		}
		if posts.Add(1) > 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeDaemonJSON(t, w, api.Error{Status: 503, Code: "redaction_busy"})
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
		close(canceled)
	}))
	defer daemon.Close()
	lease := exportTestLease(t, daemon.URL)
	_, schema := publishNumberedPDFSchemas()
	handler := batesWriteToolHandler(lease, "publish_numbered_pdf", mustResolveSchema(schema), slog.Default())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	raw, err := json.Marshal(publicationArguments(f))
	require.NoError(t, err)
	done := make(chan *sdkmcp.CallToolResult, 1)
	go func() {
		result, e := handler(ctx, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "publish_numbered_pdf", Arguments: raw}})
		if e != nil {
			t.Error(e)
		}
		done <- result
	}()
	<-entered
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPackageWrites: true}, lease)
	read := callToolResult(t, server, "get_bates_export", map[string]any{"allocation_id": testBatesAllocationID})
	require.Equal(t, f.receipt.BlobSHA256, objectField(t, read, "structuredContent")["blob_sha256"])
	busy := callToolResult(t, server, "publish_numbered_pdf", publicationArguments(f))
	require.Equal(t, "redaction_busy", objectField(t, busy, "structuredContent")["code"])
	require.Equal(t, int32(2), posts.Load())
	cancel()
	result := <-done
	require.NotNil(t, result)
	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "bates_outcome_unknown")
	<-canceled
}

func TestPublishNumberedPDF(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	for _, writer := range []string{"docbank-fresh-objects/v2", "docbank-fresh-objects/v3"} {
		for _, transport := range []string{"stdio", "http"} {
			t.Run(writer+"/"+transport, func(t *testing.T) {
				f := reviewedBatesInputs(t, writer)
				// Publication checks metadata; the real daemon qualification checks the PDF.
				f.bindPDF(t, []byte("recorded PDF identity"))
				var posts atomic.Int32
				daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, "/api/v1/redaction/publications", r.URL.Path)
					assert.Equal(t, http.MethodPost, r.Method)
					posts.Add(1)
					var input processing.RedactionRenderRequest
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &input)) {
						return
					}
					assert.Equal(t, uuid.MustParse(testBatesAllocationID), input.AllocationID)
					assert.Equal(t, f.review.SHA256, input.ReviewSHA256)
					assert.Equal(t, f.review.Selection.Source, input.Preparation.Selection.Source)
					if !assert.Len(t, input.Masks, 1) {
						return
					}
					if !assert.NotNil(t, input.Masks[0].Selector.Span) {
						return
					}
					assert.Equal(t, "text", input.Masks[0].Selector.Kind)
					assert.Equal(t, int64(6), input.Masks[0].Selector.Span.End)
					writeDaemonJSON(t, w, struct {
						api.BatesExport

						Schema string `json:"$schema"`
					}{f.receipt, "https://example.com/schema"})
				}))
				defer daemon.Close()
				server := newBatesToolTestServer(t, daemon.URL, true)
				result := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{
					"name": "publish_numbered_pdf", "arguments": publicationArguments(f),
				}))
				content := objectField(t, result, "structuredContent")
				require.Equal(t, testBatesAllocationID, content["artifact_id"])
				require.Equal(t, "verified", content["state"])
				require.Equal(t, "private", content["cacheScope"])
				require.Equal(t, int32(1), posts.Load())
			})
		}
	}
}

func TestPublishNumberedPDFAdmission(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	var requests atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected daemon request", http.StatusInternalServerError)
	}))
	defer daemon.Close()
	for _, options := range []ServerOptions{{}, {AllowProcessing: true}, {AllowExportWrites: true}} {
		server := newServerWithOptionsAndDaemon(testImplementation(), options, exportTestLease(t, daemon.URL))
		reply := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
			"name": "publish_numbered_pdf", "arguments": publicationArguments(f),
		}))
		require.NotZero(t, decodeWireError(t, reply).Code)
	}
	server := newBatesToolTestServer(t, daemon.URL, true)
	for field, value := range map[string]any{"retain_original": true, "start_at": 1, "namespace_id": uuid.New().String(),
		"review_path": nil, "review_sha256": "", "masks_path": ""} {
		args := maps.Clone(publicationArguments(f))
		args[field] = value
		reply := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "publish_numbered_pdf", "arguments": args}))
		require.NotZero(t, decodeWireError(t, reply).Code, field)
	}
	args := publicationArguments(f)
	args["review_sha256"] = testProfileID
	result := callToolResult(t, server, "publish_numbered_pdf", args)
	require.Equal(t, "redaction_review_changed", objectField(t, result, "structuredContent")["code"])
	require.Equal(t, int32(0), requests.Load())
}
