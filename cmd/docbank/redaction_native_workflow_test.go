package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"image"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/BurntSushi/toml"
	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
)

func TestNativePDFRedactionCLI(t *testing.T) {
	if !pagerender.SupportedPlatform() {
		t.Skip("page runtime requires Linux amd64")
	}
	if os.Getenv("DOCBANK_TEST_PDFTOPPM") == "" {
		if _, err := exec.LookPath("pdftoppm"); err != nil {
			t.Skip("Poppler unavailable")
		}
	}
	const child = "DOCBANK_TEST_NATIVE_REDACTION_CLI"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		executable, err := os.Executable()
		require.NoError(t, err)
		command := exec.CommandContext(ctx, executable, "-test.run=^TestNativePDFRedactionCLI$", "-test.v", "-test.timeout=5m")
		command.Env = append(os.Environ(), child+"=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		t.Log(string(output))
		return
	}
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	var config bytes.Buffer
	require.NoError(t, toml.NewEncoder(&config).Encode(map[string]any{"page_runtime": redactiontest.PageProfile(t)}))
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), config.Bytes(), 0600))
	stop := startServe(t)
	waitForDaemon(t, root)
	t.Run("review retained version", qualifyNativeRedactionCLI)
	t.Run("pages spans and gaps", qualifyNativeRedactionPages)
	t.Run("ordered gap", qualifyNativeRedactionOrderedGap)
	stop()
	assertRedactionLeavesNoJobs(t, root)
	spool, err := os.ReadDir(filepath.Join(root, "blobs", "tmp"))
	require.NoError(t, err)
	require.Empty(t, spool)
}

type nativeRedactionFiles struct {
	dir, preparation, masks, review string
	prep                            processing.RedactionPreparation
}

func importNativeRedaction(t *testing.T, data []byte, name string) nativeRedactionFiles {
	t.Helper()
	dir := t.TempDir()
	input := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(input, data, 0600))
	_, err := runCLI(t, "add", input, "--dest", "/redaction")
	require.NoError(t, err)
	encoded, err := runCLI(t, "redaction", "prepare", "/redaction/"+name, "--json")
	require.NoError(t, err)
	f := nativeRedactionFiles{dir: dir, preparation: filepath.Join(dir, "preparation.json"), masks: filepath.Join(dir, "masks.json"), review: filepath.Join(dir, "review.json")}
	require.NoError(t, os.WriteFile(f.preparation, []byte(encoded), 0600))
	require.NoError(t, json.Unmarshal([]byte(encoded), &f.prep))
	return f
}

func (f nativeRedactionFiles) resolve(t *testing.T, masks []processing.RedactionMask) processing.RedactionReview {
	t.Helper()
	encoded, err := json.Marshal(masks)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.masks, encoded, 0600))
	output, err := runCLI(t, "redaction", "resolve", f.preparation, f.masks, "--json")
	require.NoError(t, err, output)
	require.NoError(t, os.WriteFile(f.review, []byte(output), 0600))
	var review processing.RedactionReview
	require.NoError(t, json.Unmarshal([]byte(output), &review))
	return review
}

func (f nativeRedactionFiles) render(t *testing.T, name string) (string, processing.RedactionArchive) {
	t.Helper()
	path := filepath.Join(f.dir, name)
	output, err := runCLI(t, "redaction", "render", f.preparation, f.masks, f.review, path, "--json")
	require.NoError(t, err, output)
	var result processing.RedactionArchive
	require.NoError(t, json.Unmarshal([]byte(output), &result))
	entries, receipt := readNativeRedactionZIP(t, path)
	require.Equal(t, result.Receipt, receipt)
	archive, err := os.ReadFile(path)
	require.NoError(t, err)
	require.EqualValues(t, len(archive), result.Size)
	require.Equal(t, redactionPDFHash(archive), result.SHA256)
	require.NotEmpty(t, entries["redacted.pdf"])
	return path, result
}

func readNativeRedactionZIP(t *testing.T, path string) (map[string][]byte, processing.RedactionReceipt) {
	t.Helper()
	archive, err := zip.OpenReader(path)
	require.NoError(t, err)
	defer func() { _ = archive.Close() }()
	require.Len(t, archive.File, 3)
	files := map[string][]byte{}
	for i, name := range []string{"redacted.pdf", "redacted.txt", "receipt.json"} {
		entry := archive.File[i]
		require.Equal(t, name, entry.Name)
		require.Equal(t, zip.Store, entry.Method)
		reader, err := entry.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		files[name] = data
	}
	var receipt processing.RedactionReceipt
	require.NoError(t, json.Unmarshal(files["receipt.json"], &receipt, json.RejectUnknownMembers(true)))
	for name, digest := range map[string]processing.RedactionFileDigest{"redacted.pdf": receipt.PDF, "redacted.txt": receipt.Text} {
		require.EqualValues(t, len(files[name]), digest.Size)
		require.Equal(t, redactionPDFHash(files[name]), digest.SHA256)
	}
	return files, receipt
}

func qualifyNativeRedactionCLI(t *testing.T) {
	t.Helper()
	fixture := redactionFixture{font: "Helvetica", x: 72, y: 72, text: "SECRET KEEP SECRET", mask: redaction.Box{X0: 9000, Y0: 7800, X1: 17000, Y1: 11000}, black: image.Rect(280, 240, 505, 325), retained: []image.Rectangle{image.Rect(515, 240, 655, 325), image.Rect(660, 240, 875, 325)}}
	data := redactionSourcePDF(t, fixture)
	f := importNativeRedaction(t, data, "native.pdf")
	require.Empty(t, f.prep.Map.Gaps)
	fixture.mask.Page, fixture.mask.FrameSHA256 = 1, f.prep.Map.Pages[0].FrameSHA256
	masks := []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "rectangle", MapSHA256: f.prep.Map.SHA256, Boxes: []redaction.Box{fixture.mask}}}}
	qualifyNativeExpansion(t, f)
	review := f.resolve(t, masks)
	changedMasks := append([]processing.RedactionMask(nil), masks...)
	changedMasks[0].ID = uuid.New()
	encoded, err := json.Marshal(changedMasks)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.masks, encoded, 0600))
	_, err = runCLI(t, "redaction", "render", f.preparation, f.masks, f.review, filepath.Join(f.dir, "changed.zip"))
	require.Equal(t, exitStale, commandExitCode(err, true))
	encoded, err = json.Marshal(masks)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.masks, encoded, 0600))
	path, result := f.render(t, "first.zip")
	files, _ := readNativeRedactionZIP(t, path)
	require.Equal(t, "[REDACTED] KEEP SECRET\f", string(files["redacted.txt"]))
	require.Equal(t, "[REDACTED] KEEP SECRET", strings.Join(strings.Fields(readRedactionPDFText(t, files["redacted.pdf"])), " "))
	engine, err := pdfproduction.NewPDFium(review.Recipe)
	require.NoError(t, err)
	raster, err := engine.Render(t.Context(), pdfproduction.Source{Reader: bytes.NewReader(data), SHA256: f.prep.Selection.Source.SHA256, Size: int64(len(data)), StagingDirectory: t.TempDir()}, review.Resolved.Pages[0], review.Recipe)
	require.NoError(t, err)
	require.NoError(t, engine.Close())
	assertRedactionPixels(t, files["redacted.pdf"], raster.Pixels, fixture)
	for _, args := range [][]string{{"tag", "create", "pending"}, {"tag", "assign", "pending", "/redaction/native.pdf"}, {"mkdir", "/reviewed"}, {"mv", "/redaction/native.pdf", "/reviewed/renamed.pdf"}} {
		_, err := runCLI(t, args...)
		require.NoError(t, err)
	}
	replacement := writeSourceFile(t, "replacement.txt", "new current source")
	_, err = runCLI(t, "put", replacement, "/reviewed/renamed.pdf")
	require.NoError(t, err)
	historical, after := f.render(t, "historical.zip")
	require.Greater(t, after.Receipt.AdmittedNodeRevision, result.Receipt.AdmittedNodeRevision)
	require.Equal(t, result.Receipt.ReviewSHA256, after.Receipt.ReviewSHA256)
	oldFiles, _ := readNativeRedactionZIP(t, historical)
	require.Equal(t, files["redacted.pdf"], oldFiles["redacted.pdf"])
	_, err = runCLI(t, "rm", "/reviewed/renamed.pdf")
	require.NoError(t, err)
	refused := filepath.Join(f.dir, "refused.zip")
	_, err = runCLI(t, "redaction", "render", f.preparation, f.masks, f.review, refused)
	require.Equal(t, exitNotFound, commandExitCode(err, true))
	_, err = os.Stat(refused)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = runCLI(t, "restore", fmt.Sprintf("id:%d", f.prep.Selection.NodeID))
	require.NoError(t, err)
	_, err = runCLI(t, "versions", "prune", "/reviewed/renamed.pdf", "--keep-newest", "1", "--run")
	require.NoError(t, err)
	_, err = runCLI(t, "redaction", "render", f.preparation, f.masks, f.review, refused)
	require.Equal(t, exitNotFound, commandExitCode(err, true))
}

func qualifyNativeRedactionPages(t *testing.T) {
	t.Helper()
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetFont("Helvetica", "", 12)
	pdf.SetAutoPageBreak(false, 0)
	pdf.AddPage()
	pdf.Text(72, 72, "SECRET KEEP SECRET")
	pdf.AddPage()
	pdf.Text(72, 72, "SECOND PAGE SECRET")
	pdf.Rect(200, 200, 20, 20, "F")
	var data bytes.Buffer
	require.NoError(t, pdf.Output(&data))
	f := importNativeRedaction(t, data.Bytes(), "pages.pdf")
	require.Len(t, f.prep.Frames, 2)
	require.NotEmpty(t, f.prep.Map.Gaps)
	for _, gap := range f.prep.Map.Gaps {
		require.True(t, gap.Unordered)
		require.Equal(t, 2, gap.Box.Page)
	}
	masks := []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "text", MapSHA256: f.prep.Map.SHA256, Span: &redaction.Span{End: 6}}}}
	encoded, err := json.Marshal(masks)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.masks, encoded, 0600))
	output, err := runCLI(t, "redaction", "resolve", f.preparation, f.masks, "--json")
	require.Error(t, err)
	require.Contains(t, err.Error(), "mapping_incomplete")
	require.NotContains(t, output, "native-pdf-redaction-review")
	masks = append(masks, processing.RedactionMask{ID: uuid.New(), Selector: redaction.Selector{Kind: "page", MapSHA256: f.prep.Map.SHA256, Pages: []int{2}}})
	f.resolve(t, masks)
	path, _ := f.render(t, "pages.zip")
	files, receipt := readNativeRedactionZIP(t, path)
	require.Equal(t, 2, receipt.PageCount)
	require.Equal(t, "[REDACTED] KEEP SECRET\f[REDACTED]\f", string(files["redacted.txt"]))
	pages := readRedactionPDFPages(t, files["redacted.pdf"])
	for i := range pages {
		pages[i] = strings.Join(strings.Fields(pages[i]), " ")
	}
	require.Equal(t, []string{"[REDACTED] KEEP SECRET", "[REDACTED]"}, pages)
}

func qualifyNativeRedactionOrderedGap(t *testing.T) {
	t.Helper()
	data := redactiontest.FontPDF("BT /F1 24 Tf 72 720 Td (A) Tj ET", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding /ToUnicode 6 0 R >>", "1 begincodespacerange <00> <FF> endcodespacerange 1 beginbfchar <41> <0020> endbfchar")
	f := importNativeRedaction(t, data, "gap.pdf")
	require.NotEmpty(t, f.prep.Map.Gaps)
	ordered := false
	for _, gap := range f.prep.Map.Gaps {
		ordered = ordered || !gap.Unordered
	}
	require.True(t, ordered)
	missing := []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "rectangle", MapSHA256: f.prep.Map.SHA256, Boxes: []redaction.Box{{Page: 1, FrameSHA256: f.prep.Map.Pages[0].FrameSHA256, X0: 30000, Y0: 30000, X1: 31000, Y1: 31000}}}}}
	encoded, err := json.Marshal(missing)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.masks, encoded, 0600))
	_, err = runCLI(t, "redaction", "resolve", f.preparation, f.masks, "--json")
	require.ErrorContains(t, err, "mapping_incomplete")

	f.resolve(t, []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "page", MapSHA256: f.prep.Map.SHA256, Pages: []int{1}}}})
	path, _ := f.render(t, "gap.zip")
	files, _ := readNativeRedactionZIP(t, path)
	require.Equal(t, "[REDACTED]\f", string(files["redacted.txt"]))
}

func qualifyNativeExpansion(t *testing.T, f nativeRedactionFiles) {
	t.Helper()
	box := f.prep.Map.Atoms[0].Boxes[0]
	box.X1 = box.X0 + (box.X1-box.X0)/2
	masks := []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "rectangle", MapSHA256: f.prep.Map.SHA256, Boxes: []redaction.Box{box}}}}
	data, err := json.Marshal(masks)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.masks, data, 0600))
	cmd := newRedactionResolveCommand()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{f.preparation, f.masks, "--json"})
	var out, diagnostic bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostic)
	err = cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	require.Empty(t, out.String())
	var proposal *redaction.Problem
	require.ErrorAs(t, err, &proposal)
	require.NotNil(t, proposal.Expanded)
	require.Contains(t, diagnostic.String(), "expanded_boxes")
	masks[0].Selector = *proposal.Expanded
	f.resolve(t, masks)
}

func assertRedactionLeavesNoJobs(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "docbank.db")
	if info, err := os.Stat(path + "-wal"); err == nil {
		require.Zero(t, info.Size(), "immutable inspection requires a checkpointed database")
	} else {
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	db, err := store.DefaultSQLiteDriver().Open(path, sqlite.OpenOptions{Access: sqlite.ReadOnlyImmutable, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT
 (SELECT COUNT(*) FROM rendition_jobs) + (SELECT COUNT(*) FROM embedding_jobs) +
 (SELECT COUNT(*) FROM export_jobs)`).Scan(&count))
	require.Zero(t, count, "redaction must not enqueue or retain jobs")
}
