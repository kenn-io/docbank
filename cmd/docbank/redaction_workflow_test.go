package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/mcp"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestNativePDFPreparationCLI(t *testing.T) {
	if !pagerender.SupportedPlatform() {
		t.Skip("page runtime requires Linux amd64")
	}
	if os.Getenv("DOCBANK_TEST_PDFTOPPM") == "" {
		if _, err := exec.LookPath("pdftoppm"); err != nil {
			t.Skip("Poppler unavailable")
		}
	}
	const childVariable = "DOCBANK_TEST_NATIVE_PREPARATION_CLI"
	if os.Getenv(childVariable) != "1" {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		executable, err := os.Executable()
		require.NoError(t, err)
		command := exec.CommandContext(ctx, executable, "-test.run=^TestNativePDFPreparationCLI$", "-test.v", "-test.timeout=2m")
		command.Env = append(os.Environ(), childVariable+"=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		t.Log(string(output))
		return
	}
	profile := redactiontest.PageProfile(t)
	home := t.TempDir()
	t.Setenv("DOCBANK_HOME", home)
	var cfg bytes.Buffer
	require.NoError(t, toml.NewEncoder(&cfg).Encode(map[string]any{"page_runtime": profile}))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), cfg.Bytes(), 0o600))
	stop := startServe(t)
	waitForDaemon(t, home)
	fixture := redactionFixture{font: "Helvetica", x: 72, y: 72, text: "SECRET KEEP SECRET",
		mask: redaction.Box{X0: 9000, Y0: 7800, X1: 17000, Y1: 11000}, black: image.Rect(280, 240, 505, 325),
		retained: []image.Rectangle{image.Rect(515, 240, 655, 325), image.Rect(660, 240, 875, 325)}}
	data := redactionSourcePDF(t, fixture)
	input := filepath.Join(t.TempDir(), "native.pdf")
	require.NoError(t, os.WriteFile(input, data, 0o600))
	_, err := runCLI(t, "add", input, "--dest", "/inbox", "--json")
	require.NoError(t, err)
	stat, err := runCLI(t, "stat", "/inbox/native.pdf", "--json")
	require.NoError(t, err)
	var node api.Node
	require.NoError(t, json.Unmarshal([]byte(stat), &node))
	encoded, err := runCLI(t, "redaction", "prepare", "/inbox/native.pdf", "--json")
	require.NoError(t, err)
	var result processing.RedactionPreparation
	require.NoError(t, json.Unmarshal([]byte(encoded), &result))
	require.NoError(t, processing.ValidateRedactionPreparation(result))
	require.Equal(t, node.ID, result.Selection.NodeID)
	require.Equal(t, node.CurrentVersionID, result.Selection.Source.VersionID)
	require.Equal(t, node.BlobHash, result.Selection.Source.SHA256)
	require.Equal(t, fixture.text, result.Map.Text)
	require.Equal(t, redaction.Span{Start: 0, End: 18}, result.Map.Pages[0].Span)
	require.Len(t, result.Map.Atoms, 16)
	require.Empty(t, result.Map.Gaps)
	require.Empty(t, result.Map.Units)
	require.Len(t, result.Frames, 1)
	require.EqualValues(t, 85000, result.Frames[0].Width)
	require.EqualValues(t, 110000, result.Frames[0].Height)
	human, err := runCLI(t, "redaction", "prepare", "/inbox/native.pdf")
	require.NoError(t, err)
	require.Contains(t, human, "16 atoms, 0 ordered gaps, 0 unordered gaps")
	require.Contains(t, human, "Ordered gaps need covering masks; unordered gaps need whole-page masks.")
	require.NotContains(t, human, fixture.text)
	_, err = runCLI(t, "redaction", "prepare", "/inbox")
	require.ErrorContains(t, err, `"/inbox" is a directory`)
	require.Equal(t, exitNotFound, commandExitCode(err, true))
	_, err = runCLI(t, "add", input, "--dest", "/other")
	require.NoError(t, err)
	_, err = runCLI(t, "redaction", "prepare", "/other/native.pdf", "--version", node.CurrentVersionID)
	require.ErrorContains(t, err, `does not belong to "/other/native.pdf"`)
	require.Equal(t, exitNotFound, commandExitCode(err, true))
	_, err = runCLI(t, "redaction", "prepare", "/missing.pdf")
	require.Equal(t, exitNotFound, commandExitCode(err, true))
	replacement := writeSourceFile(t, "replacement.txt", "replacement text")
	_, err = runCLI(t, "put", replacement, "/inbox/native.pdf")
	require.NoError(t, err)
	historical, err := runCLI(t, "redaction", "prepare", "/inbox/native.pdf", "--version", node.CurrentVersionID, "--json")
	require.NoError(t, err)
	var old processing.RedactionPreparation
	require.NoError(t, json.Unmarshal([]byte(historical), &old))
	require.Equal(t, result.Map, old.Map)
	_, err = runCLI(t, "redaction", "prepare", "/inbox/native.pdf")
	require.Equal(t, exitGeneral, commandExitCode(err, true))
	qualifyMCPHistoricalPreparation(t)
	stop()
	assertNoProcessingJobs(t, home, "preparation must not enqueue processing")
	// Only the daemon-returned map supplies geometry and evidence to resolution.
	fixture.mask.Page, fixture.mask.FrameSHA256 = 1, result.Map.Pages[0].FrameSHA256
	plan, err := redaction.Resolve(result.Map, "redact_selected", []redaction.Decision{{
		ID: "00000000-0000-4000-8000-000000000001", MemberID: "00000000-0000-4000-8000-000000000002",
		Action: "redact", Selector: redaction.Selector{Kind: "rectangle", MapSHA256: result.Map.SHA256,
			Boxes: []redaction.Box{fixture.mask}}}}, pdfproduction.QualifiedRecipe())
	require.NoError(t, err)
	sidecar, err := redaction.Text(plan)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED] KEEP SECRET\f", string(sidecar))
	output := filepath.Join(t.TempDir(), "redacted.pdf")
	artifact, original := writeRedactionPDF(t, output, pdfproduction.Source{
		Reader: bytes.NewReader(data), Size: int64(len(data)), SHA256: node.BlobHash, StagingDirectory: t.TempDir(),
	}, plan)
	saved, err := os.ReadFile(output)
	require.NoError(t, err)
	verifyRedactionPDF(t, saved, sidecar, artifact)
	require.Equal(t, "[REDACTED] KEEP SECRET", strings.Join(strings.Fields(readRedactionPDFText(t, saved)), " "))
	assertRedactionPixels(t, saved, original, fixture)
}

func qualifyMCPHistoricalPreparation(t *testing.T) {
	t.Helper()
	const path = "/inbox/gap.pdf"
	// This glyph has visible ink but maps to whitespace, leaving an ordered gap.
	pdf := redactiontest.FontPDF("BT /F1 24 Tf 72 720 Td (A) Tj ET", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding /ToUnicode 6 0 R >>", "1 begincodespacerange <00> <FF> endcodespacerange 1 beginbfchar <41> <0020> endbfchar")
	file := filepath.Join(t.TempDir(), "gap.pdf")
	require.NoError(t, os.WriteFile(file, pdf, 0600))
	_, err := runCLI(t, "add", file, "--dest", "/inbox")
	require.NoError(t, err)
	stat, err := runCLI(t, "stat", path, "--json")
	require.NoError(t, err)
	var node api.Node
	require.NoError(t, json.Unmarshal([]byte(stat), &node))
	replacement := writeSourceFile(t, "new-current.txt", "current text is not the historical PDF")
	_, err = runCLI(t, "put", replacement, path)
	require.NoError(t, err)

	agent := startWorkflowMCP(t, mcp.ServerOptions{AllowPackageWrites: true})
	defer agent.stop(t)
	var info api.VaultIdentity
	require.NoError(t, json.Unmarshal(agent.call(t, "get_vault_info", map[string]any{}), &info))
	var versions struct {
		Items []struct {
			ContentVersionID string `json:"content_version_id"`
			BlobHash         string `json:"blob_hash"`
			Size             int64  `json:"size"`
			IsCurrent        bool   `json:"is_current"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(agent.call(t, "list_document_versions", map[string]any{"node_id": node.ID}), &versions))
	require.Len(t, versions.Items, 2)
	var historicalVersionFound bool
	for _, version := range versions.Items {
		if version.ContentVersionID == node.CurrentVersionID {
			require.False(t, version.IsCurrent)
			require.Equal(t, node.BlobHash, version.BlobHash)
			require.Equal(t, node.Size, version.Size)
			historicalVersionFound = true
		} else {
			require.True(t, version.IsCurrent)
		}
	}
	require.True(t, historicalVersionFound)
	f := nativeRedactionFiles{dir: t.TempDir()}
	f.preparation = filepath.Join(f.dir, "PRIVATE-preparation.json")
	f.masks = filepath.Join(f.dir, "masks.json")
	f.review = filepath.Join(f.dir, "review.json")
	raw := agent.call(t, "prepare_native_pdf", map[string]any{"vault_id": info.VaultID, "node_id": node.ID,
		"content_version_id": node.CurrentVersionID, "blob_hash": node.BlobHash, "size": node.Size, "destination_path": f.preparation})
	var receipt struct {
		State            string `json:"state"`
		ContentVersionID string `json:"content_version_id"`
		SHA256           string `json:"sha256"`
		Pages            []struct {
			Ordered   int `json:"ordered_gaps"`
			Unordered int `json:"unordered_gaps"`
		} `json:"pages"`
	}
	require.NoError(t, json.Unmarshal(raw, &receipt))
	require.Equal(t, "published", receipt.State, string(raw))
	require.Equal(t, node.CurrentVersionID, receipt.ContentVersionID)
	require.Len(t, receipt.Pages, 1)
	require.Equal(t, 1, receipt.Pages[0].Ordered)
	require.Zero(t, receipt.Pages[0].Unordered)
	saved, err := os.ReadFile(f.preparation)
	require.NoError(t, err)
	require.Equal(t, redactionPDFHash(saved), receipt.SHA256)
	require.NoError(t, json.Unmarshal(saved, &f.prep))
	uncovered := processing.RedactionMask{ID: uuid.New(), Selector: redaction.Selector{Kind: "rectangle", MapSHA256: f.prep.Map.SHA256,
		Boxes: []redaction.Box{{Page: 1, FrameSHA256: f.prep.Map.Pages[0].FrameSHA256, X0: 30000, Y0: 30000, X1: 31000, Y1: 31000}}}}
	masks, err := json.Marshal([]processing.RedactionMask{uncovered})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.masks, masks, 0600))
	_, err = runCLI(t, "redaction", "resolve", f.preparation, f.masks, "--json")
	require.ErrorContains(t, err, "mapping_incomplete")
	review := f.resolve(t, []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "page", MapSHA256: f.prep.Map.SHA256, Pages: []int{1}}}})
	text, err := redaction.Text(review.Resolved)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED]\f", string(text))
}
