package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
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
	"time"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

type reviewedBatesFixture struct {
	arguments map[string]any
	paths     map[string]string
	review    processing.RedactionReview
	numbered  processing.NumberedRecipe
	receipt   api.BatesExport
}

func reviewedBatesInputs(t *testing.T, writer string) reviewedBatesFixture {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile("../../cmd/docbank/testdata/redaction-preparation.json")
	require.NoError(t, err)
	var preparation processing.RedactionPreparation
	require.NoError(t, json.Unmarshal(data, &preparation))
	masks := []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "text", MapSHA256: preparation.Map.SHA256, Span: &redaction.Span{End: 6}}}}
	recipe := pdfproduction.QualifiedRecipe()
	recipe.WriterVersion = writer
	review, err := processing.BuildRedactionReview(preparation, masks, preparation.Selection.Revision, recipe)
	require.NoError(t, err)
	namespace := store.BatesNamespace{NamespaceID: testBatesNamespaceID, Prefix: "OUR", Padding: 6}
	numbered, err := processing.BuildNumberedRecipe(uuid.New(), namespace, 41, review)
	require.NoError(t, err)
	paths := map[string]string{"destination_path": filepath.Join(dir, "delivery.pdf")}
	args := map[string]any{"allocation_id": testBatesAllocationID, "destination_path": paths["destination_path"], "overwrite": false}
	for key, value := range map[string]any{"preparation_path": preparation, "masks_path": masks, "review_path": review} {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		path := filepath.Join(dir, key+".json")
		require.NoError(t, os.WriteFile(path, data, 0600))
		args[key], paths[key] = path, path
	}
	encoded, err := canonical.Marshal(numbered)
	require.NoError(t, err)
	request, err := numbered.PlanRequest(uuid.MustParse(testBatesSnapshotID))
	require.NoError(t, err)
	requestSHA, err := store.BatesRequestDigest(request)
	require.NoError(t, err)
	page := store.BatesArtifactPage{Ordinal: 1, OccurrenceID: request.Pages[0].OccurrenceID, SourceBlobSHA256: preparation.Selection.Source.SHA256, SourcePage: 1, OutputPage: 1, Label: "OUR000041"}
	receipt := api.BatesExport{
		ArtifactID: testBatesAllocationID, AllocationID: testBatesAllocationID,
		MediaType: "application/pdf", PageCount: 1, State: "verified",
		RecipeSHA256: request.RecipeSHA256, Recipe: encoded,
		Pages: []store.BatesArtifactPage{page}, CreatedAt: "2026-10-01T00:00:00Z",
		Namespace: &api.BatesNamespace{NamespaceID: testBatesNamespaceID, Prefix: "OUR",
			Padding: 6, CreatedAt: "2026-10-01T00:00:00Z"},
		Allocation: &api.BatesAllocation{
			AllocationID: testBatesAllocationID, NamespaceID: testBatesNamespaceID,
			SnapshotID: testBatesSnapshotID, RequestSHA256: requestSHA,
			RecipeSHA256: request.RecipeSHA256, State: "committed", StartSequence: 41, EndSequence: 41,
			CreatedAt: "2026-10-01T00:00:00Z",
			Labels: []api.BatesPageLabel{{Ordinal: 1, OccurrenceID: page.OccurrenceID,
				SourcePage: 1, OutputPage: 1, Label: page.Label}},
		},
	}
	return reviewedBatesFixture{arguments: args, paths: paths, review: review, numbered: numbered, receipt: receipt}
}

func (f *reviewedBatesFixture) bindPDF(t *testing.T, data []byte) {
	t.Helper()
	f.receipt.Size = int64(len(data))
	f.receipt.BlobSHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
	manifest, err := store.BatesArtifactManifestSHA(store.BatesArtifact{
		ArtifactID: f.receipt.ArtifactID, AllocationID: f.receipt.AllocationID,
		BlobSHA256: f.receipt.BlobSHA256, Size: f.receipt.Size, MediaType: f.receipt.MediaType,
		PageCount: 1, RecipeSHA256: f.receipt.RecipeSHA256, Pages: f.receipt.Pages,
	})
	require.NoError(t, err)
	f.receipt.ManifestSHA256 = manifest
}

type reviewedBatesPages struct{ page *pdfproduction.PageArtifact }

func (p *reviewedBatesPages) Next(context.Context) (pdfproduction.PageArtifact, error) {
	if p.page == nil {
		return pdfproduction.PageArtifact{}, io.EOF
	}
	a := *p.page
	p.page = nil
	return a, nil
}

func (f *reviewedBatesFixture) pdf(t *testing.T, alter bool) []byte {
	t.Helper()
	source := f.review.Resolved.Pages[0]
	pixels := image.NewNRGBA(image.Rect(0, 0, 2550, 3300))
	draw.Draw(pixels, pixels.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	raster, err := pdfproduction.Burn(pdfproduction.Raster{Pixels: pixels, Page: source, DPI: 300}, f.review.Resolved.RedactBoxes, f.review.Recipe)
	require.NoError(t, err)
	presentation, err := f.numbered.Presentation()
	require.NoError(t, err)
	p := presentation[0]
	raster, err = pdfproduction.Endorse(raster, p.Layout, p.Endorsements, f.review.Recipe)
	require.NoError(t, err)
	if alter {
		masks, err := pdfproduction.PlanPixels(source, f.review.Resolved.RedactBoxes, f.review.Recipe)
		require.NoError(t, err)
		raster.Pixels.SetNRGBA(masks[0].Min.X+1, masks[0].Min.Y+1, color.NRGBA{R: 255, A: 255})
	}
	var pngData bytes.Buffer
	require.NoError(t, png.Encode(&pngData, raster.Pixels))
	data := pngData.Bytes()
	layout, err := canonical.Marshal(p.Layout)
	require.NoError(t, err)
	endorsements, err := canonical.Marshal(p.Endorsements)
	require.NoError(t, err)
	a := pdfproduction.PageArtifact{Page: p.Layout.Output, Layout: p.Layout, LayoutSHA256: fmt.Sprintf("%x", sha256.Sum256(layout)), ResolvedSHA256: f.review.Resolved.SHA256, Runs: f.review.Resolved.Runs, Endorsements: p.Endorsements, EndorsementsSHA256: fmt.Sprintf("%x", sha256.Sum256(endorsements)), PNGSHA256: fmt.Sprintf("%x", sha256.Sum256(data)), PNGSize: int64(len(data)), OpenPNG: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}
	var out bytes.Buffer
	pages := &reviewedBatesPages{page: &a}
	options := pdfproduction.PDFWriteOptions{Recipe: f.review.Recipe, StagingDirectory: t.TempDir(), MaxBytes: 512 << 20}
	if alter {
		err = pdfproduction.WriteRendition(t.Context(), &out, pages, options)
	} else {
		err = pdfproduction.WriteFresh(t.Context(), &out, pages, f.review.Resolved, options)
	}
	require.NoError(t, err)
	return out.Bytes()
}

// Full-mode tests are serial: admission and PDFium backing belong to the process.
func TestReviewedBatesExport(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	for _, writer := range []string{"docbank-fresh-objects/v2", "docbank-fresh-objects/v3"} {
		t.Run(writer, func(t *testing.T) {
			f := reviewedBatesInputs(t, writer)
			data := f.pdf(t, false)
			f.bindPDF(t, data)
			daemon := httptest.NewServer(reviewedBatesHandler(t, f, data))
			t.Cleanup(daemon.Close)
			server := newBatesToolTestServer(t, daemon.URL, true)
			transport := "stdio"
			if writer == "docbank-fresh-objects/v3" {
				transport = "http"
			}
			result := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{
				"name": "export_bates_file", "arguments": f.arguments,
			}))
			content := objectField(t, result, "structuredContent")
			require.Equal(t, "masks_text_and_numbers", content["verification"])
			saved, err := os.ReadFile(f.paths["destination_path"])
			require.NoError(t, err)
			require.Equal(t, data, saved)
		})
	}
}

func TestReviewedBatesExportRequiresAllPaths(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	server := newBatesToolTestServer(t, "http://127.0.0.1:1", true)
	for _, value := range []any{nil, ""} {
		f.arguments["masks_path"] = value
		response := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_bates_file", "arguments": f.arguments}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, response).Code)
	}
	delete(f.arguments, "masks_path")
	response := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_bates_file", "arguments": f.arguments}))
	require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, response).Code)
}

func reviewedBatesHandler(t *testing.T, f reviewedBatesFixture, data []byte) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/bates/exports/" + testBatesAllocationID:
			writeDaemonJSON(t, w, f.receipt)
		case "/api/v1/bates/exports/" + testBatesAllocationID + "/content":
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}
}

func TestReviewedBatesExportRejectsChangedPixels(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	data := f.pdf(t, true)
	f.bindPDF(t, data) // All unsigned receipt bindings agree with the modified PDF.
	daemon := httptest.NewServer(reviewedBatesHandler(t, f, data))
	defer daemon.Close()
	server := newBatesToolTestServer(t, daemon.URL, true)
	path := f.paths["destination_path"]
	require.NoError(t, os.WriteFile(path, []byte("keep existing"), 0600))
	f.arguments["overwrite"] = true
	output := exportCall(t, server, "export_bates_file", f.arguments)
	require.Equal(t, "redaction_integrity", output["code"])
	saved, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "keep existing", string(saved))
	// Verification failure released the gate: a new call reaches input validation.
	require.NoError(t, os.Remove(f.paths["review_path"]))
	response := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_bates_file", "arguments": f.arguments}))
	require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, response).Code)
}

func TestReviewedBatesExportAdmissionAndCancellation(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	data := f.pdf(t, false)
	f.bindPDF(t, data)
	destination := f.paths["destination_path"]
	require.NoError(t, os.WriteFile(destination, []byte("keep existing"), 0600))
	f.arguments["overwrite"] = true
	entered := make(chan struct{})
	var requests, transfers atomic.Int32
	serve := reviewedBatesHandler(t, f, data)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.HasSuffix(r.URL.Path, "/content") && transfers.Add(1) == 1 {
			_, _ = w.Write(data[:1])
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Error(err)
			}
			close(entered)
			<-r.Context().Done()
			return
		}
		serve(w, r)
	}))
	defer daemon.Close()
	lease := exportTestLease(t, daemon.URL)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPackageWrites: true}, lease)
	raw, err := json.Marshal(f.arguments)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, outputSchema := exportBatesFileSchemas()
	handler := batesWriteToolHandler(lease, "export_bates_file", mustResolveSchema(outputSchema), slog.Default())
	type outcome struct {
		result *sdkmcp.CallToolResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := handler(ctx, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{
			Name: "export_bates_file", Arguments: raw,
		}})
		done <- outcome{result: result, err: err}
	}()
	<-entered
	before := requests.Load()
	second := maps.Clone(f.arguments)
	second["review_path"] = filepath.Join(t.TempDir(), "unread.json")
	otherSession := newBatesToolTestServer(t, daemon.URL, true)
	output := exportCall(t, otherSession, "export_bates_file", second)
	require.Equal(t, "redaction_busy", output["code"])
	require.Equal(t, before, requests.Load(), "busy admission must not access the daemon")
	stages, err := filepath.Glob(filepath.Join(filepath.Dir(f.paths["destination_path"]), ".docbank-bates-*"))
	require.NoError(t, err)
	require.Len(t, stages, 1)
	// Recovery and unrelated reads use neither the full-check gate nor PDFium.
	recovery := map[string]any{"allocation_id": testBatesAllocationID, "destination_path": filepath.Join(t.TempDir(), "recovery.pdf"), "overwrite": false}
	require.Equal(t, "recorded_bytes_and_numbers", exportCall(t, server, "export_bates_file", recovery)["verification"])
	require.Equal(t, testBatesAllocationID, objectField(t, callToolResult(t, server, "get_bates_export", map[string]any{"allocation_id": testBatesAllocationID}), "structuredContent")["allocation_id"])
	cancel()
	finished := <-done
	require.NoError(t, finished.err)
	canceledOutput, ok := finished.result.StructuredContent.(toolErrorOutput)
	require.True(t, ok)
	require.Equal(t, "redaction_canceled", canceledOutput.Code)
	saved, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, "keep existing", string(saved))
	stages, err = filepath.Glob(filepath.Join(filepath.Dir(f.paths["destination_path"]), ".docbank-bates-*"))
	require.NoError(t, err)
	require.Empty(t, stages)
	f.arguments["review_path"] = filepath.Join(t.TempDir(), "unread.json")
	response := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_bates_file", "arguments": f.arguments}))
	require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, response).Code)
}

func TestReviewedBatesExportCleanupFailure(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	data := f.pdf(t, false)
	f.bindPDF(t, data)
	daemon := httptest.NewServer(reviewedBatesHandler(t, f, data))
	defer daemon.Close()
	cleanup := cleanupBatesStage
	t.Cleanup(func() { cleanupBatesStage = cleanup })
	cleanupBatesStage = func(stage *filepublish.Stage) error {
		// Admission remains closed until cleanup returns, even after publication.
		raw, err := json.Marshal(f.arguments)
		require.NoError(t, err)
		var input exportBatesFileInput
		require.NoError(t, json.Unmarshal(raw, &input))
		_, err = exportReviewedBatesFile(t.Context(), nil, input, slog.Default())
		code, _ := stableDomainError(err)
		require.Equal(t, "redaction_busy", code)
		return errors.Join(cleanup(stage), errors.New("synthetic cleanup failure"))
	}
	var logs bytes.Buffer
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPackageWrites: true, Logger: slog.New(slog.NewTextHandler(&logs, nil))}, exportTestLease(t, daemon.URL))
	require.Equal(t, "published", exportCall(t, server, "export_bates_file", f.arguments)["state"])
	require.Contains(t, logs.String(), "synthetic cleanup failure")
	saved, err := os.ReadFile(f.paths["destination_path"])
	require.NoError(t, err)
	require.Equal(t, data, saved)
	// Cleanup failure must not strand admission; invalid input remains an input error.
	f.arguments["destination_path"] = filepath.Join(t.TempDir(), "another.pdf")
	require.NoError(t, os.Remove(f.paths["review_path"]))
	raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_bates_file", "arguments": f.arguments}))
	require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
}

func TestReviewedBatesExportPrivateInputErrors(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	var logs bytes.Buffer
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPackageWrites: true, Logger: slog.New(slog.NewTextHandler(&logs, nil))}, exportTestLease(t, "http://127.0.0.1:1"))
	const secret = "SYNTHETIC PRIVATE PHRASE"
	for _, input := range []string{`{"` + secret + `":true}`, `{"contract":"` + secret + `"}`} {
		require.NoError(t, os.WriteFile(f.paths["preparation_path"], []byte(input), 0600))
		response := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_bates_file", "arguments": f.arguments}))
		require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, response).Code)
		require.NotContains(t, string(response), secret)
		require.NotContains(t, logs.String(), secret)
	}
}

func TestReviewedBatesErrorClassification(t *testing.T) {
	live := t.Context()
	canceled, cancel := context.WithCancel(live)
	cancel()
	expired, expire := context.WithDeadline(live, time.Time{})
	defer expire()
	memory := &pdfproduction.RSSLimitError{ObservedBytes: 2 << 30, LimitBytes: 1 << 30}
	for _, test := range []struct {
		name string
		ctx  context.Context
		err  error
		code string
	}{
		{"memory", live, memory, "redaction_memory"},
		{"caller canceled before memory", canceled, memory, "redaction_canceled"},
		{"caller expired before memory", expired, memory, "redaction_timeout"},
		{"inner deadline", live, context.DeadlineExceeded, "redaction_worker_timeout"},
		{"inner cancellation", live, context.Canceled, ""},
		{"version", live, pdfproduction.ErrVerifierVersion, "redaction_verifier_version"},
		{"changed review", live, processing.ErrRedactionReviewChanged, "redaction_review_changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, _ := stableDomainError(reviewedBatesDownloadError(test.ctx, test.err))
			require.Equal(t, test.code, code)
		})
	}
}

func TestReviewedBatesExportPreflight(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	for _, mode := range []string{"changed masks", "different review", "preparation version", "review version", "ordinary stamp"} {
		t.Run(mode, func(t *testing.T) {
			f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
			f.bindPDF(t, []byte("not reached"))
			require.NoError(t, os.WriteFile(f.paths["destination_path"], []byte("keep existing"), 0600))
			f.arguments["overwrite"] = true
			var downloads atomic.Int32
			if mode == "ordinary stamp" {
				_, f.receipt = syntheticStampedBatesExport(t)
			}
			switch mode {
			case "changed masks":
				var masks []processing.RedactionMask
				raw, err := os.ReadFile(f.paths["masks_path"])
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(raw, &masks))
				masks[0].Selector.Span.End = 7
				raw, err = json.Marshal(masks)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.paths["masks_path"], raw, 0600))
			case "different review":
				// The private files are internally valid; only the retained allocation differs.
				// Use another valid reviewed preparation instead of editing a review digest.
				var prep processing.RedactionPreparation
				raw, err := os.ReadFile(f.paths["preparation_path"])
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(raw, &prep))
				prep.Selection.NodeID++
				var masks []processing.RedactionMask
				raw, err = os.ReadFile(f.paths["masks_path"])
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(raw, &masks))
				changed, err := processing.BuildRedactionReview(prep, masks, prep.Selection.Revision, f.review.Recipe)
				require.NoError(t, err)
				for key, value := range map[string]any{"preparation_path": prep, "review_path": changed} {
					raw, err := json.Marshal(value)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(f.paths[key], raw, 0600))
				}
			case "preparation version":
				var prep processing.RedactionPreparation
				raw, err := os.ReadFile(f.paths["preparation_path"])
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(raw, &prep))
				prep.Evidence.RendererSHA256 = strings.Repeat("a", 64)
				raw, err = json.Marshal(prep)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.paths["preparation_path"], raw, 0600))
			case "review version":
				f.review.Recipe.WriterVersion = "unknown-writer"
				raw, err := json.Marshal(f.review)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.paths["review_path"], raw, 0600))
			}
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/content") {
					downloads.Add(1)
				}
				writeDaemonJSON(t, w, f.receipt)
			}))
			defer daemon.Close()
			server := newBatesToolTestServer(t, daemon.URL, true)
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_bates_file", "arguments": f.arguments}))
			if mode == "ordinary stamp" {
				require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
			} else {
				expected := "redaction_review_changed"
				if strings.HasSuffix(mode, "version") {
					expected = "redaction_verifier_version"
				}
				require.Equal(t, expected, objectField(t, decodeResult(t, raw), "structuredContent")["code"])
			}
			require.Zero(t, downloads.Load())
			saved, err := os.ReadFile(f.paths["destination_path"])
			require.NoError(t, err)
			require.Equal(t, "keep existing", string(saved))
		})
	}
}

func TestReviewedBatesExportPublicationConflict(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	data := f.pdf(t, false)
	f.bindPDF(t, data)
	destination := f.paths["destination_path"]
	serve := reviewedBatesHandler(t, f, data)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/content") {
			if err := os.WriteFile(destination, []byte("concurrent file"), 0600); err != nil {
				t.Error(err)
				http.Error(w, "synthetic write failed", http.StatusInternalServerError)
				return
			}
		}
		serve(w, r)
	}))
	defer daemon.Close()
	var logs bytes.Buffer
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowPackageWrites: true, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}, exportTestLease(t, daemon.URL))
	output := exportCall(t, server, "export_bates_file", f.arguments)
	require.Equal(t, "bates_local_io", output["code"])
	var diagnostic struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(logs.Bytes(), &diagnostic))
	require.Contains(t, diagnostic.Error, destination, "destination I/O keeps its diagnostic")
	saved, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, "concurrent file", string(saved))
	entries, err := os.ReadDir(filepath.Dir(destination))
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".docbank-bates-")
	}
}

func TestReviewedBatesExportInputLimits(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "home"))
	f := reviewedBatesInputs(t, "docbank-fresh-objects/v3")
	var requests atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer daemon.Close()
	server := newBatesToolTestServer(t, daemon.URL, true)
	for field, limit := range map[string]int{"preparation_path": 16 << 20, "masks_path": 1 << 20, "review_path": 16 << 20} {
		t.Run(field, func(t *testing.T) {
			original, err := os.ReadFile(f.paths[field])
			require.NoError(t, err)
			// Valid JSON beyond the bound: decoding alone would accept it.
			oversized := append(bytes.Clone(original), bytes.Repeat([]byte(" "), limit+1-len(original))...)
			require.NoError(t, os.WriteFile(f.paths[field], oversized, 0600))
			response := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "export_bates_file", "arguments": f.arguments}))
			require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, response).Code)
			require.Zero(t, requests.Load())
			require.NoError(t, os.WriteFile(f.paths[field], original, 0600))
		})
	}
}
