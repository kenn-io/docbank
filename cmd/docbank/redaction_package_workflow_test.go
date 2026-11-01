package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/BurntSushi/toml"
	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

func TestNumberedRecipientPackageCLI(t *testing.T) {
	if !pagerender.SupportedPlatform() {
		t.Skip("page runtime requires Linux amd64")
	}
	if os.Getenv("DOCBANK_TEST_PDFTOPPM") == "" {
		if _, err := exec.LookPath("pdftoppm"); err != nil {
			t.Skip("Poppler unavailable")
		}
	}
	const child = "DOCBANK_TEST_RECIPIENT_PACKAGE"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		binary, err := os.Executable()
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestNumberedRecipientPackageCLI$", "-test.v", "-test.timeout=5m")
		cmd.Env = append(os.Environ(), child+"=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		t.Log(string(out))
		return
	}
	qualifyRecipientPackage(t)
}

func qualifyRecipientPackage(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DOCBANK_HOME", home)
	var cfg bytes.Buffer
	require.NoError(t, toml.NewEncoder(&cfg).Encode(map[string]any{"server": map[string]string{"api_key": "recipient-test-key"}, "page_runtime": redactiontest.PageProfile(t)}))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), cfg.Bytes(), 0600))
	stop := startServe(t)
	waitForDaemon(t, home)
	out, err := runCLI(t, "bates", "namespaces", "--create", "--prefix", "CASE", "--padding", "6", "--json")
	require.NoError(t, err)
	var ns api.BatesNamespace
	require.NoError(t, json.Unmarshal([]byte(out), &ns))
	root := t.TempDir()
	selection := recipientSelection{Contract: "native-pdf-recipient-selection/v1"}
	var expectedPDFs, expectedTexts [][]byte
	var lastSnapshot string
	for i, texts := range [][]string{{"SECRET KEEP SECRET", "SECRET SECOND PAGE"}, {"SECRET THIRD PAGE"}} {
		pdf := fpdf.New("P", "pt", "Letter", "")
		pdf.SetFont("Helvetica", "", 12)
		for _, text := range texts {
			pdf.AddPage()
			pdf.Text(72, 72, text)
		}
		var src bytes.Buffer
		require.NoError(t, pdf.Output(&src))
		f := importNativeRedaction(t, src.Bytes(), fmt.Sprintf("source-%d.pdf", i))
		masks := make([]processing.RedactionMask, len(texts))
		for j, p := range f.prep.Map.Pages {
			masks[j] = processing.RedactionMask{ID: uuid.New(), Selector: redaction.Selector{Kind: "text", MapSHA256: f.prep.Map.SHA256, Span: &redaction.Span{Start: p.Span.Start, End: p.Span.Start + 6}}}
		}
		f.resolve(t, masks)
		if i == 1 {
			recipe := pdfproduction.QualifiedRecipe()
			recipe.WriterVersion = "docbank-fresh-objects/v2"
			review, err := processing.BuildRedactionReview(f.prep, masks, f.prep.Selection.Revision, recipe)
			require.NoError(t, err)
			encoded, err := json.Marshal(review)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(f.review, encoded, 0600))
			stop()
			seedRecipientV2Reservation(t, home, ns, 200, review)
			stop = startServe(t)
			waitForDaemon(t, home)
		}
		start := []string{"101", "200"}[i]
		out, err = runCLI(t, "redaction", "reserve", f.preparation, f.masks, f.review, "--namespace", ns.NamespaceID, "--start", start, "--retain-original", "--json")
		require.NoError(t, err, out)
		var allocation processing.NumberedReservation
		require.NoError(t, json.Unmarshal([]byte(out), &allocation))
		lastSnapshot = allocation.SnapshotID.String()
		archive := filepath.Join(f.dir, "delivery.zip")
		out, err = runCLI(t, "redaction", "render", f.preparation, f.masks, f.review, archive, "--allocation", allocation.AllocationID.String(), "--json")
		require.NoError(t, err, out)
		files, _ := readNativeRedactionZIP(t, archive)
		expectedPDFs = append(expectedPDFs, files["redacted.pdf"])
		expectedTexts = append(expectedTexts, files["redacted.txt"])
		selection.Documents = append(selection.Documents, recipientInput{allocation.AllocationID.String(), f.preparation, f.masks, f.review})
	}
	stampedID := recipientStampedFixture(t, lastSnapshot, ns.NamespaceID)
	// Selection order is independent of reservation order and preserves label gaps.
	slices.Reverse(selection.Documents)
	slices.Reverse(expectedPDFs)
	slices.Reverse(expectedTexts)
	for _, args := range [][]string{{"rm", "/redaction/source-0.pdf"}, {"rm", "/redaction/source-1.pdf"}, {"trash", "empty", "--run"}, {"gc", "--run"}} {
		_, err = runCLI(t, args...)
		require.NoError(t, err)
	}

	stop()
	// Restart without the page runtime. Retained PDFs, not source inspection, feed packaging.
	var config map[string]any
	require.NoError(t, toml.Unmarshal(cfg.Bytes(), &config))
	delete(config, "page_runtime")
	cfg.Reset()
	require.NoError(t, toml.NewEncoder(&cfg).Encode(config))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), cfg.Bytes(), 0600))
	stop = startServe(t)
	runtime := waitForDaemon(t, home)
	recipientAdmissionChecks(t, "http://"+runtime.Address, "recipient-test-key", selection, stampedID)
	encoded, err := json.Marshal(selection)
	require.NoError(t, err)
	selectionPath := filepath.Join(root, "selection.json")
	require.NoError(t, os.WriteFile(selectionPath, encoded, 0600))
	destination := filepath.Join(root, "recipient.zip")
	before, err := runCLI(t, "bates", "export", "history", "--json")
	require.NoError(t, err)
	started := time.Now()
	out, err = runCLI(t, "redaction", "package", selectionPath, destination, "--json")
	require.NoError(t, err, out)
	after, err := runCLI(t, "bates", "export", "history", "--json")
	require.NoError(t, err)
	require.Equal(t, before, after)
	var result recipientPackageResult
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Equal(t, "masks_text_and_numbers", result.Verification)
	require.Equal(t, 3, result.Receipt.PageCount)
	data, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, redactionPDFHash(data), result.SHA256)
	require.Equal(t, int64(len(data)), result.Size)
	t.Logf("package: %d bytes, %s", result.Size, time.Since(started))
	_, err = runCLI(t, "redaction", "package", selectionPath, destination)
	require.Error(t, err)
	again, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, data, again)
	stages, err := filepath.Glob(filepath.Join(root, "docbank-recipient-*"))
	require.NoError(t, err)
	require.Empty(t, stages)
	stop()
	archive, err := os.Open(destination)
	require.NoError(t, err)
	extracted, err := loadfile.ExtractZIP(t.Context(), archive, result.Size)
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	defer func() { require.NoError(t, loadfile.RemoveExtractedZIP(extracted)) }()
	root = extracted
	for i := range expectedPDFs {
		pdf, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("VOL001/PDF/%06d.pdf", i+1)))
		require.NoError(t, err)
		require.Equal(t, expectedPDFs[i], pdf)
		text, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("VOL001/TEXT/%06d.txt", i+1)))
		require.NoError(t, err)
		require.Equal(t, expectedTexts[i], text)
	}
	require.Equal(t, "[REDACTED] THIRD PAGE\f", string(expectedTexts[0]))
	require.Equal(t, "[REDACTED] KEEP SECRET\f[REDACTED] SECOND PAGE\f", string(expectedTexts[1]))
	fresh := t.TempDir()
	t.Setenv("DOCBANK_HOME", fresh)
	stop = startServe(t)
	waitForDaemon(t, fresh)

	out, err = runCLI(t, "package", "preflight", root, "--profile", "dat-concordance-v1", "--page-map-profile", "opt-standard-v1", "--encoding", "utf-8", "--map", filepath.Join(root, "mapping.json"), "--json")
	require.NoError(t, err, out)
	var pre api.PackagePreflight
	require.NoError(t, json.Unmarshal([]byte(out), &pre))
	require.False(t, pre.Blocking, out)
	require.Equal(t, 2, pre.Records)
	out, err = runCLI(t, "package", "import", pre.PreflightID, "--name", "recipient", "--index-supplied-text", "--json")
	require.NoError(t, err, out)
	var job api.PackageImportJob
	require.NoError(t, json.Unmarshal([]byte(out), &job))
	require.Eventually(t, func() bool {
		out, err = runCLI(t, "package", "import", "status", job.OperationID, "--json")
		require.NoError(t, err, out)
		require.NoError(t, json.Unmarshal([]byte(out), &job))
		return job.State == "complete" || job.State == "partial" || job.State == "failed"
	}, time.Minute, 50*time.Millisecond)
	require.Equal(t, "complete", job.State, out)
	out, err = runCLI(t, "package", "members", job.PackageID, "--json")
	require.NoError(t, err, out)
	var members api.PackageMemberPage
	require.NoError(t, json.Unmarshal([]byte(out), &members))
	require.Len(t, members.Items, 2)
	recipient, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	for i, member := range members.Items {
		require.Equal(t, redactionPDFHash(expectedPDFs[i]), member.BlobSHA256)
		roles := map[string]bool{}
		for _, representation := range member.Representations {
			if representation.Role != "produced_pdf" && representation.Role != "supplied_text" {
				continue
			}
			stream, err := recipient.VersionContent(t.Context(), representation.ContentVersionID)
			require.NoError(t, err)
			content, err := io.ReadAll(stream)
			require.NoError(t, err)
			require.NoError(t, stream.Close())
			expected := expectedPDFs[i]
			if representation.Role == "supplied_text" {
				expected = expectedTexts[i]
				require.Equal(t, "supplied", representation.TextAuthority)
			}
			require.Equal(t, expected, content)
			roles[representation.Role] = true
		}
		require.True(t, roles["produced_pdf"])
		require.True(t, roles["supplied_text"])
	}
	require.NoError(t, recipient.Close())
	for _, label := range []string{"CASE000101", "CASE000102", "CASE000200"} {
		out, err = runCLI(t, "labels", "lookup", label, "--package", job.PackageID, "--json")
		require.NoError(t, err, out)
		require.Contains(t, out, label)
		var matches api.PackageLabelCandidatePage
		require.NoError(t, json.Unmarshal([]byte(out), &matches))
		sets := map[string]bool{}
		for _, m := range matches.Items {
			require.Equal(t, "received", m.Provenance)
			sets[m.LabelSet] = true
			if m.LabelSet == "received" {
				page := 1
				if label == "CASE000102" {
					page = 2
				}
				require.Equal(t, page, m.PageNumber)
			}
		}
		require.True(t, sets["received"])
		require.True(t, sets[ns.NamespaceID], "all three fixture labels are range endpoints")
	}
	out, err = runCLI(t, "bates", "namespaces", "--json")
	require.NoError(t, err)
	var namespaces api.BatesNamespacePage
	require.NoError(t, json.Unmarshal([]byte(out), &namespaces))
	require.Empty(t, namespaces.Items)
	stop()
}

func recipientAdmissionChecks(t *testing.T, address, key string, selection recipientSelection, stampedID string) {
	t.Helper()
	target, err := url.Parse(address)
	require.NoError(t, err)
	var downloads atomic.Int32
	var failDownload atomic.Int32
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target); r.Out.Host = target.Host }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/content") && strings.HasPrefix(r.URL.Path, "/api/v1/bates/exports/") {
			n := downloads.Add(1)
			if failDownload.Load() == n {
				http.Error(w, "injected delivery interruption", http.StatusServiceUnavailable)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	c := daemonconn.New(server.URL, key)
	defer func() { _ = c.Close() }()
	stamped := selection
	stamped.Documents = slices.Clone(selection.Documents)
	stamped.Documents[1].AllocationID = stampedID
	_, err = preflightRecipientPackage(t.Context(), c, stamped)
	require.Equal(t, exitUsage, commandExitCode(recipientCLIError(err), true))
	require.Zero(t, downloads.Load())
	original, err := os.ReadFile(selection.Documents[1].Review)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(selection.Documents[1].Review, []byte("{"), 0600))
	_, err = preflightRecipientPackage(t.Context(), c, selection)
	require.Equal(t, exitUsage, commandExitCode(err, true))
	require.Zero(t, downloads.Load())
	require.NoError(t, os.WriteFile(selection.Documents[1].Review, original, 0600))
	wrong := selection
	wrong.Documents = slices.Clone(selection.Documents)
	wrong.Documents[1] = selection.Documents[0]
	wrong.Documents[1].AllocationID = selection.Documents[1].AllocationID
	_, err = preflightRecipientPackage(t.Context(), c, wrong)
	require.Equal(t, exitStale, commandExitCode(recipientCLIError(err), true))
	require.Zero(t, downloads.Load())
	checked, err := preflightRecipientPackage(t.Context(), c, selection)
	require.NoError(t, err)
	// No racing process: production preflight has completed before this real edit.
	masks, err := os.ReadFile(selection.Documents[0].Masks)
	require.NoError(t, err)
	var changed []processing.RedactionMask
	require.NoError(t, json.Unmarshal(masks, &changed))
	changed[0].ID = uuid.New()
	changedBytes, err := json.Marshal(changed)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(selection.Documents[0].Masks, changedBytes, 0600))
	var out bytes.Buffer
	_, err = writeRecipientPackage(t.Context(), c, checked, t.TempDir(), &out)
	require.Equal(t, exitStale, commandExitCode(recipientCLIError(err), true))
	require.Zero(t, downloads.Load())
	require.NoError(t, os.WriteFile(selection.Documents[0].Masks, masks, 0600))
	// An actual first download succeeds; the next transfer fails. Publication and staging are cleaned up.
	failDownload.Store(2)
	destination := filepath.Join(t.TempDir(), "failed.zip")
	cmd := newRedactionPackageCommand()
	cmd.SetOut(io.Discard)
	err = publishRecipientPackage(t.Context(), cmd, destination, true, c, checked)
	require.Error(t, err)
	require.EqualValues(t, 2, downloads.Load())
	require.NoFileExists(t, destination)
	entries, err := os.ReadDir(filepath.Dir(destination))
	require.NoError(t, err)
	require.Empty(t, entries)
	failDownload.Store(0)
	single := recipientPreflight{Inputs: checked.Inputs[:1], Receipts: checked.Receipts[:1], Documents: checked.Documents[:1]}
	cmd.SetOut(searchFailingWriter{})
	err = publishRecipientPackage(t.Context(), cmd, destination, true, c, single)
	require.ErrorContains(t, err, "already saved at")
	require.ErrorContains(t, err, "output closed")
	require.FileExists(t, destination)
	entries, err = os.ReadDir(filepath.Dir(destination))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

// A real pending reservation is the only supported way to finish a v2 delivery.
// Seed the pre-upgrade ledger through its normal transaction while the daemon is stopped.
func seedRecipientV2Reservation(t *testing.T, home string, namespace api.BatesNamespace, start int64, review processing.RedactionReview) {
	t.Helper()
	catalog, err := store.Open(filepath.Join(home, "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, catalog.Close()) }()
	recipe, err := processing.BuildNumberedRecipe(uuid.MustParse(catalog.VaultID()), store.BatesNamespace{NamespaceID: namespace.NamespaceID, Prefix: namespace.Prefix, Padding: namespace.Padding}, start, review)
	require.NoError(t, err)
	request, err := recipe.PlanRequest(uuid.New())
	require.NoError(t, err)
	request.OperationID = uuid.New().String()
	source := review.Selection.Source
	occurrence := request.Pages[0].OccurrenceID
	snapshot := store.SnapshotSealRequest{SnapshotID: request.SnapshotID, Members: []store.CollectionSnapshotMember{{
		Ordinal: 1, OccurrenceID: occurrence, NodeID: review.Selection.NodeID, ContentVersionID: source.VersionID, BlobSHA256: source.SHA256, Size: source.Size, FamilyID: occurrence, FamilyOrder: 1, DisplayName: "original.pdf", FrozenFieldsJSON: "{}", DocumentKind: "other", SelectedSourcePages: []int{1}, SelectedPDFSHA256: source.SHA256, SourcePageCount: 1,
		Representations: []store.CollectionSnapshotRepresentation{{OccurrenceID: occurrence, Role: "native", Status: "available", TextAuthority: "none", ContentVersionID: source.VersionID, BlobSHA256: source.SHA256, MediaType: "application/pdf", Size: source.Size, VerifiedPageCount: 1}},
	}}}
	_, err = catalog.ReserveSnapshotBatesRange(t.Context(), snapshot, request)
	require.NoError(t, err)
}

func recipientStampedFixture(t *testing.T, snapshot, namespace string) string {
	t.Helper()
	recipe := filepath.Join(t.TempDir(), "stamp.json")
	out, err := runCLI(t, "bates", "plan", snapshot, "--namespace", namespace, "--start-at", "500", "--recipe-out", recipe, "--json")
	require.NoError(t, err, out)
	out, err = runCLI(t, "bates", "reserve", snapshot, "--recipe", recipe, "--operation-id", uuid.New().String(), "--json")
	require.NoError(t, err, out)
	var allocation api.BatesAllocation
	require.NoError(t, json.Unmarshal([]byte(out), &allocation))
	out, err = runCLI(t, "bates", "export", "run", allocation.AllocationID, "--recipe", recipe, "--json")
	require.NoError(t, err, out)
	return allocation.AllocationID
}
