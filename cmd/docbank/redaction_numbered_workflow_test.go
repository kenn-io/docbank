package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/BurntSushi/toml"
	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/mcp"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

func TestNumberedNativePDFCLI(t *testing.T) {
	if !pagerender.SupportedPlatform() {
		t.Skip("page runtime requires Linux amd64")
	}
	if os.Getenv("DOCBANK_TEST_PDFTOPPM") == "" {
		if _, err := exec.LookPath("pdftoppm"); err != nil {
			t.Skip("Poppler unavailable")
		}
	}
	const child = "DOCBANK_TEST_NUMBERED_NATIVE_PDF"
	if os.Getenv(child) == "" {
		for _, version := range []string{"v2", "v3"} {
			t.Run(version, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
				defer cancel()
				binary, err := os.Executable()
				require.NoError(t, err)
				cmd := exec.CommandContext(ctx, binary, "-test.run=^TestNumberedNativePDFCLI$", "-test.v", "-test.timeout=5m")
				cmd.Env = append(os.Environ(), child+"="+version)
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", out)
				t.Log(string(out))
			})
		}
		return
	}

	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	configuration := map[string]any{"page_runtime": redactiontest.PageProfile(t),
		"server": map[string]string{"api_key": "synthetic-numbered-key"},
		"backup": map[string]string{"repo": filepath.Join(t.TempDir(), "backup")}}
	writeConfig := func() {
		var out bytes.Buffer
		require.NoError(t, toml.NewEncoder(&out).Encode(configuration))
		require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), out.Bytes(), 0600))
	}
	writeConfig()
	stop := startServe(t)
	waitForDaemon(t, root)
	_, err := runCLI(t, "backup", "init")
	require.NoError(t, err)
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetFont("Helvetica", "", 12)
	for _, text := range []string{"SECRET KEEP SECRET", "SECRET SECOND PAGE"} {
		pdf.AddPage()
		pdf.Text(72, 72, text)
	}
	var source bytes.Buffer
	require.NoError(t, pdf.Output(&source))
	f := importNativeRedaction(t, source.Bytes(), "two-pages.pdf")
	masks := make([]processing.RedactionMask, 2)
	for i, page := range f.prep.Map.Pages {
		masks[i] = processing.RedactionMask{ID: uuid.New(), Selector: redaction.Selector{
			Kind: "text", MapSHA256: f.prep.Map.SHA256, Span: &redaction.Span{Start: page.Span.Start, End: page.Span.Start + 6}}}
	}
	f.resolve(t, masks)
	var historical processing.RedactionReview
	if os.Getenv(child) == "v2" {
		// Retain the reviewed recipe across the upgrade; no writer-selection flag.
		recipe := pdfproduction.QualifiedRecipe()
		recipe.WriterVersion = "docbank-fresh-objects/v2"
		historical, err = processing.BuildRedactionReview(
			f.prep, masks, f.prep.Selection.Revision, recipe)
		require.NoError(t, err)
		data, err := json.Marshal(historical)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(f.review, data, 0600))
	}
	out, err := runCLI(t, "bates", "namespaces", "--create", "--prefix", "QUAL", "--padding", "6", "--json")
	require.NoError(t, err)
	var namespace api.BatesNamespace
	require.NoError(t, json.Unmarshal([]byte(out), &namespace))
	reserve := []string{"redaction", "reserve", f.preparation, f.masks, f.review,
		"--namespace", namespace.NamespaceID, "--start", "1", "--retain-original", "--json"}
	if os.Getenv(child) == "v2" {
		_, err = runCLI(t, reserve...)
		require.Equal(t, exitStale, commandExitCode(err, true))
		require.ErrorContains(t, err, "resolve again")
		plain := filepath.Join(f.dir, "obsolete.zip")
		_, err = runCLI(t, "redaction", "render", f.preparation, f.masks, f.review, plain)
		require.Equal(t, exitStale, commandExitCode(err, true))
		require.ErrorContains(t, err, "resolve again")
		_, err = os.Stat(plain)
		require.ErrorIs(t, err, os.ErrNotExist)

		// Seed a pending pre-upgrade reservation through the real storage contract.
		// The daemon is stopped; no admission bypass is exposed to callers.
		stop()
		catalog, err := store.Open(t.Context(), filepath.Join(root, "docbank.db"))
		require.NoError(t, err)
		ns := store.BatesNamespace{NamespaceID: namespace.NamespaceID, Prefix: "QUAL", Padding: 6}
		recipe, err := processing.BuildNumberedRecipe(
			uuid.MustParse(catalog.VaultID()), ns, 1, historical)
		require.NoError(t, err)
		request, err := recipe.PlanRequest(uuid.New())
		require.NoError(t, err)
		request.OperationID = uuid.New().String()
		_, err = catalog.ReplaySnapshotBatesRange(t.Context(), request)
		require.ErrorIs(t, err, store.ErrNotFound, "refused reservation must not retain numbers")
		source := f.prep.Selection.Source
		occurrence := request.Pages[0].OccurrenceID
		snapshot := store.SnapshotSealRequest{SnapshotID: request.SnapshotID,
			Members: []store.CollectionSnapshotMember{{
				Ordinal: 1, OccurrenceID: occurrence, NodeID: f.prep.Selection.NodeID,
				ContentVersionID: source.VersionID, BlobSHA256: source.SHA256, Size: source.Size,
				FamilyID: occurrence, FamilyOrder: 1, DisplayName: "original.pdf",
				FrozenFieldsJSON: "{}", DocumentKind: "other", SelectedSourcePages: []int{1, 2},
				SelectedPDFSHA256: source.SHA256, SourcePageCount: 2,
				Representations: []store.CollectionSnapshotRepresentation{{
					OccurrenceID: occurrence, Role: "native", Status: "available", TextAuthority: "none",
					ContentVersionID: source.VersionID, BlobSHA256: source.SHA256,
					MediaType: "application/pdf", Size: source.Size, VerifiedPageCount: 2,
				}},
			}}}
		_, err = catalog.ReserveSnapshotBatesRange(t.Context(), snapshot, request)
		require.NoError(t, err)
		require.NoError(t, catalog.Close())
		stop = startServe(t)
		waitForDaemon(t, root)
	}
	out, err = runCLI(t, reserve...)
	require.NoError(t, err, out)
	var reservation processing.NumberedReservation
	require.NoError(t, json.Unmarshal([]byte(out), &reservation))
	require.Equal(t, []string{"QUAL000001", "QUAL000002"}, []string{reservation.Pages[0].Label, reservation.Pages[1].Label})
	for _, args := range [][]string{
		{"tag", "create", "pending"}, {"tag", "assign", "pending", "/redaction/two-pages.pdf"},
		{"mkdir", "/reviewed"}, {"mv", "/redaction/two-pages.pdf", "/reviewed/renamed.pdf"},
	} {
		_, err := runCLI(t, args...)
		require.NoError(t, err)
	}
	out, err = runCLI(t, reserve...)
	require.NoError(t, err, out)
	var afterMetadata processing.NumberedReservation
	require.NoError(t, json.Unmarshal([]byte(out), &afterMetadata))
	require.Equal(t, reservation, afterMetadata)
	stop()
	stop = startServe(t)
	waitForDaemon(t, root)
	for _, args := range [][]string{{"rm", "/reviewed/renamed.pdf"}, {"trash", "empty", "--run"}, {"gc", "--run"}} {
		_, err := runCLI(t, args...)
		require.NoError(t, err)
	}
	out, err = runCLI(t, reserve...)
	require.NoError(t, err, out)
	var afterTrash processing.NumberedReservation
	require.NoError(t, json.Unmarshal([]byte(out), &afterTrash))
	require.Equal(t, reservation, afterTrash)
	// Drop the real daemon's first success after it commits. Only an explicit
	// retry may publish again, including for a pending historical allocation.
	savedReview, err := os.ReadFile(f.review)
	require.NoError(t, err)
	var selected processing.RedactionReview
	require.NoError(t, json.Unmarshal(savedReview, &selected))
	publicationArgs := map[string]any{"allocation_id": reservation.AllocationID.String(),
		"review_sha256": selected.SHA256, "preparation_path": f.preparation,
		"masks_path": f.masks, "review_path": f.review}
	runtimeRecord := waitForDaemon(t, root)
	publicationTarget, err := url.Parse("http://" + runtimeRecord.Address)
	require.NoError(t, err)
	var posts atomic.Int32
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(publicationTarget); r.Out.Host = publicationTarget.Host },
		ModifyResponse: func(r *http.Response) error {
			if r.Request.URL.Path == "/api/v1/redaction/publications" && posts.Load() == 1 && r.StatusCode == http.StatusOK {
				_, err := io.Copy(io.Discard, r.Body)
				assert.NoError(t, err)
				assert.NoError(t, r.Body.Close())
				return errors.New("injected lost publication reply")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			hijacker, ok := w.(http.Hijacker)
			if !assert.True(t, ok) {
				return
			}
			connection, _, err := hijacker.Hijack()
			if assert.NoError(t, err) {
				assert.NoError(t, connection.Close())
			}
		},
	}
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/redaction/publications" {
			posts.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	defer relay.Close()
	connection := daemonconn.New(relay.URL, "synthetic-numbered-key")
	defer func() { require.NoError(t, connection.Close()) }()
	publisher := startWorkflowMCPServer(t, mcp.NewServerWithConnection(connection, mcp.ServerOptions{AllowPackageWrites: true}))
	lost := publisher.call(t, "publish_numbered_pdf", publicationArgs)
	var failure struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(lost, &failure))
	require.Equal(t, "bates_outcome_unknown", failure.Code)
	require.Equal(t, int32(1), posts.Load())
	var retained api.BatesExport
	require.NoError(t, json.Unmarshal(publisher.call(t, "get_bates_export", map[string]any{"allocation_id": reservation.AllocationID.String()}), &retained))
	var published api.BatesExport
	require.NoError(t, json.Unmarshal(publisher.call(t, "publish_numbered_pdf", publicationArgs), &published))
	require.Equal(t, retained, published)
	require.Equal(t, int32(2), posts.Load())
	require.Equal(t, "QUAL000001", published.Pages[0].Label)
	publisher.stop(t)

	archive := filepath.Join(f.dir, "delivery.zip")
	render := []string{"redaction", "render", f.preparation, f.masks, f.review, archive,
		"--allocation", reservation.AllocationID.String(), "--json"}
	out, err = runCLI(t, render...)
	require.NoError(t, err, out)
	var rendered processing.RedactionArchive
	require.NoError(t, json.Unmarshal([]byte(out), &rendered))
	files, receipt := readNativeRedactionZIP(t, archive)
	require.Equal(t, "docbank-fresh-objects/"+os.Getenv(child), receipt.Numbered.Reservation.Recipe.Renderer.WriterVersion)
	require.Equal(t, "[REDACTED] KEEP SECRET\f[REDACTED] SECOND PAGE\f", string(files["redacted.txt"]))
	require.Equal(t, reservation.AllocationID, receipt.Numbered.Reservation.AllocationID)
	pages := readRedactionPDFPages(t, files["redacted.pdf"])
	require.Equal(t, []string{"[REDACTED] KEEP SECRET\r\nQUAL000001", "[REDACTED] SECOND PAGE\r\nQUAL000002"}, pages)
	_, err = runCLI(t, "redaction", "verify", f.preparation, f.masks, f.review, archive, "--json")
	require.NoError(t, err)
	checked := filepath.Join(f.dir, "full.pdf")
	out, err = runCLI(t, "bates", "export", "download", reservation.AllocationID.String(), checked,
		"--preparation", f.preparation, "--masks", f.masks, "--review", f.review, "--json")
	require.NoError(t, err, out)
	var full daemonconn.BatesDownloadResult
	require.NoError(t, json.Unmarshal([]byte(out), &full))
	require.Equal(t, "masks_text_and_numbers", full.Verification)
	stop()
	assertNumberedOriginalRetained(t, root, f.prep, source.Bytes())
	delete(configuration, "page_runtime")
	writeConfig()
	stop = startServe(t)
	waitForDaemon(t, root)
	out, err = runCLI(t, reserve...)
	require.NoError(t, err, out)
	render = append(render, "--overwrite")
	out, err = runCLI(t, render...)
	require.NoError(t, err, out)
	var repeated processing.RedactionArchive
	require.NoError(t, json.Unmarshal([]byte(out), &repeated))
	require.Equal(t, rendered, repeated)
	// Full agent recovery uses only retained bytes and the private review files,
	// after source deletion and a renderer-free restart.
	agent := startWorkflowMCP(t, mcp.ServerOptions{AllowPackageWrites: true})
	var replayed api.BatesExport
	require.NoError(t, json.Unmarshal(agent.call(t, "publish_numbered_pdf", publicationArgs), &replayed))
	require.Equal(t, published, replayed)
	agentPath := filepath.Join(f.dir, "agent-checked.pdf")
	result := agent.call(t, "export_bates_file", map[string]any{
		"allocation_id": reservation.AllocationID.String(), "destination_path": agentPath, "overwrite": false,
		"preparation_path": f.preparation, "masks_path": f.masks, "review_path": f.review,
	})
	var agentResult struct {
		Verification string `json:"verification"`
		State        string `json:"state"`
	}
	require.NoError(t, json.Unmarshal(result, &agentResult))
	require.Equal(t, "masks_text_and_numbers", agentResult.Verification, string(result))
	require.Equal(t, "published", agentResult.State)
	agentPDF, err := os.ReadFile(agentPath)
	require.NoError(t, err)
	require.Equal(t, files["redacted.pdf"], agentPDF)
	agent.stop(t)
	for _, path := range []string{f.preparation, f.masks, f.review} {
		require.NoError(t, os.Remove(path))
	}
	recoverNumberedPDF(t, reservation, files["redacted.pdf"])
	snapshot := backupReportSnapshot(t, "numbered-native")
	target := restoreReportSnapshot(t, snapshot)
	stop()
	assertRedactionLeavesNoJobs(t, root)
	t.Setenv("DOCBANK_HOME", target)
	stop = startServe(t)
	waitForDaemon(t, target)
	recoverNumberedPDF(t, reservation, files["redacted.pdf"])
	stop()
	assertNumberedOriginalRetained(t, target, f.prep, source.Bytes())
	assertRedactionLeavesNoJobs(t, target)
	for _, home := range []string{root, target} {
		entries, err := os.ReadDir(filepath.Join(home, "blobs", "tmp"))
		require.NoError(t, err)
		require.Empty(t, entries)
	}
}

func recoverNumberedPDF(t *testing.T, r processing.NumberedReservation, expected []byte) {
	t.Helper()
	c, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, c.Close()) }()
	label := "QUAL000002"
	page, err := c.API().FindBatesExports(t.Context(), &apiclient.FindBatesExportsRequestOptions{
		Query: &apiclient.FindBatesExportsQuery{BatesLabel: &label}})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, r.AllocationID.String(), page.Items[0].AllocationID)
	path := filepath.Join(t.TempDir(), "recovered.pdf")
	out, err := runCLI(t, "bates", "export", "download", page.Items[0].AllocationID, path, "--json")
	require.NoError(t, err, out)
	var result daemonconn.BatesDownloadResult
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Equal(t, "recorded_bytes_and_numbers", result.Verification)
	require.Equal(t, redactionPDFHash(expected), result.SHA256)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, expected, data)
}

func assertNumberedOriginalRetained(t *testing.T, root string, prep processing.RedactionPreparation, expected []byte) {
	t.Helper()
	catalog, err := store.Open(t.Context(), filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, catalog.Close()) }()
	view, err := catalog.ContentVersionViewByID(t.Context(), prep.Selection.NodeID, prep.Selection.Source.VersionID)
	require.NoError(t, err)
	require.Equal(t, prep.Selection.Source.SHA256, view.Version.BlobHash)
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	defer func() { require.NoError(t, blobs.Close()) }()
	reader, _, err := blobs.OpenStreamContext(t.Context(), view.Version.BlobHash)
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.True(t, reader.Verified())
	require.Equal(t, expected, data)
}
