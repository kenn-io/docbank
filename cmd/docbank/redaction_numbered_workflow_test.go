package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/BurntSushi/toml"
	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/daemonconn"
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
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		binary, err := os.Executable()
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestNumberedNativePDFCLI$", "-test.v", "-test.timeout=5m")
		cmd.Env = append(os.Environ(), child+"=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		t.Log(string(out))
		return
	}
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	configuration := map[string]any{"page_runtime": redactiontest.PageProfile(t),
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
	out, err := runCLI(t, "bates", "namespaces", "--create", "--prefix", "QUAL", "--padding", "6", "--json")
	require.NoError(t, err)
	var namespace api.BatesNamespace
	require.NoError(t, json.Unmarshal([]byte(out), &namespace))
	reserve := []string{"redaction", "reserve", f.preparation, f.masks, f.review,
		"--namespace", namespace.NamespaceID, "--start", "1", "--retain-original", "--json"}
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
	for _, args := range [][]string{{"rm", "/reviewed/renamed.pdf"}, {"trash", "empty", "--run"}, {"gc", "--run"}} {
		_, err := runCLI(t, args...)
		require.NoError(t, err)
	}
	archive := filepath.Join(f.dir, "delivery.zip")
	render := []string{"redaction", "render", f.preparation, f.masks, f.review, archive,
		"--allocation", reservation.AllocationID.String(), "--json"}
	out, err = runCLI(t, render...)
	require.NoError(t, err, out)
	var rendered processing.RedactionArchive
	require.NoError(t, json.Unmarshal([]byte(out), &rendered))
	files, receipt := readNativeRedactionZIP(t, archive)
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
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
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
