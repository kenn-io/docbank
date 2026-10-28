package main

import (
	"archive/zip"
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
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
)

func TestRedactionVerifyCLI(t *testing.T) {
	f := newRedactionVerifyFixture(t)
	data := f.archive(t, "valid")
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	t.Setenv("TMP", scratch)
	t.Setenv("TEMP", scratch)
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	output, err := runCLI(t, append(f.args, "--json")...)
	require.NoError(t, err)
	var result processing.RedactionArchive
	require.NoError(t, json.Unmarshal([]byte(output), &result))
	require.EqualValues(t, len(data), result.Size)
	require.Equal(t, digest, result.SHA256)
	require.Equal(t, f.review.SHA256, result.Receipt.ReviewSHA256)
	require.Equal(t, f.review.Selection, result.Receipt.Selection)
	require.Equal(t, redactionFixtureDigest([]byte("[REDACTED] KEEP SECRET\f")), result.Receipt.Text)
	output, err = runCLI(t, f.args...)
	require.NoError(t, err)
	require.Contains(t, output, digest)
	require.Contains(t, output, strconv.Itoa(len(data)))
	require.Contains(t, output, "PDF masks and text match the supplied review.")
	require.Contains(t, output, "Unmasked image content and producer identity were not verified.")
	require.Contains(t, output, "Compare the archive SHA-256 with the render-time value.")
	entries, err := os.ReadDir(scratch)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestRedactionVerifyRejectsSelfConsistentPDF(t *testing.T) {
	for _, scenario := range []string{"non-PDF", "unmasked PDF"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRedactionVerifyFixture(t)
			data := f.archive(t, scenario)
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			t.Setenv("TMP", scratch)
			t.Setenv("TEMP", scratch)
			_, err := daemonconn.VerifyRedactionArchive(t.Context(), bytes.NewReader(data), int64(len(data)), f.review)
			require.NoError(t, err, "archive hashes and CRCs must agree before the stronger check")
			output, err := runCLI(t, append(f.args, "--json")...)
			require.Error(t, err)
			require.Equal(t, 6, commandExitCode(err, true))
			require.Empty(t, output)
			entries, err := os.ReadDir(scratch)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestRedactionVerifyErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		exit int
	}{
		{"malformed preparation", 2}, {"changed masks", 4}, {"review", 6},
		{"changed PDF", 6}, {"changed text", 6}, {"missing archive", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRedactionVerifyFixture(t)
			f.archive(t, test.name)
			switch test.name {
			case "malformed preparation":
				require.NoError(t, os.WriteFile(f.args[2], []byte("{"), 0600))
			case "changed masks":
				f.request.Masks[0].ID = uuid.New()
				data, err := json.Marshal(f.request.Masks)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.args[3], data, 0600))
			case "missing archive":
				require.NoError(t, os.Remove(f.args[5]))
			}
			output, err := runCLI(t, append(f.args, "--json")...)
			require.Error(t, err)
			require.Equal(t, test.exit, commandExitCode(err, true))
			require.Empty(t, output)
		})
	}
}

func TestRedactionVerifyOutputAndCancellation(t *testing.T) {
	f := newRedactionVerifyFixture(t)
	f.archive(t, "valid")
	for _, asJSON := range []bool{false, true} {
		command := newRedactionCommand()
		command.SilenceUsage, command.SilenceErrors = true, true
		args := append([]string(nil), f.args[1:]...)
		if asJSON {
			args = append(args, "--json")
		}
		command.SetArgs(args)
		failed := errors.New("synthetic output failure")
		command.SetOut(reportTestWriter(func([]byte) (int, error) { return 0, failed }))
		err := command.ExecuteContext(t.Context())
		require.ErrorIs(t, err, failed)
		require.Equal(t, 1, commandExitCode(err, true))
	}
	command := newRedactionCommand()
	command.SilenceUsage, command.SilenceErrors = true, true
	command.SetArgs(f.args[1:])
	var output bytes.Buffer
	command.SetOut(&output)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := command.ExecuteContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, commandExitCode(err, true))
	require.Empty(t, output.String())
}

type redactionVerifyFixture struct {
	args    []string
	request processing.RedactionResolveRequest
	review  processing.RedactionReview
}

func newRedactionVerifyFixture(t *testing.T) redactionVerifyFixture {
	t.Helper()
	dir := t.TempDir()
	// A file cannot be a daemon home. Success therefore exercises offline access.
	home := filepath.Join(dir, "unusable-home")
	require.NoError(t, os.WriteFile(home, nil, 0600))
	t.Setenv("DOCBANK_HOME", home)
	f := redactionVerifyFixture{args: []string{"redaction", "verify"}}
	for _, name := range []string{"preparation.json", "masks.json", "review.json", "result.zip"} {
		f.args = append(f.args, filepath.Join(dir, name))
	}
	// Captured from the real prepare path for synthetic Helvetica text, with no gaps.
	// After renderer/recipe changes, recapture f.preparation immediately after
	// importNativeRedaction in TestNativePDFRedactionRecoveryCLI.
	data, err := os.ReadFile("testdata/redaction-preparation.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &f.request.Preparation))
	require.NoError(t, os.WriteFile(f.args[2], data, 0600))
	f.request.Masks = []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{
		Kind: "text", MapSHA256: f.request.Preparation.Map.SHA256, Span: &redaction.Span{End: 6},
	}}}
	f.review, err = processing.BuildRedactionReview(f.request.Preparation, f.request.Masks,
		f.request.Preparation.Selection.Revision)
	require.NoError(t, err)
	for i, value := range []any{f.request.Masks, f.review} {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(f.args[i+3], data, 0600))
	}
	return f
}

func (f redactionVerifyFixture) archive(t *testing.T, scenario string) []byte {
	t.Helper()
	pdf := redactionVerifyPDF(t, f.review, nil, scenario)
	if scenario == "non-PDF" {
		pdf = []byte("self-consistently hashed non-PDF payload")
	}
	text := []byte("[REDACTED] KEEP SECRET\f")
	if scenario == "changed text" {
		text = []byte("SECRET KEEP SECRET\f")
	}
	receipt := processing.RedactionReceipt{
		Contract: "native-pdf-redaction-result/v1", Selection: f.review.Selection,
		AdmittedNodeRevision: 7, PreparationSHA256: f.review.PreparationSHA256,
		DecisionsSHA256: f.review.DecisionsSHA256, ReviewSHA256: f.review.SHA256,
		ResolvedSHA256: f.review.Resolved.SHA256, Recipe: f.review.Recipe,
		PageCount: 1, PDF: redactionFixtureDigest(pdf), Text: redactionFixtureDigest(text),
	}
	if scenario == "review" {
		receipt.ReviewSHA256 = strings.Repeat("0", 64)
	}
	if scenario == "changed PDF" {
		pdf = []byte("altered PDF payload")
	}
	encoded, err := json.Marshal(receipt)
	require.NoError(t, err)
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for i, name := range []string{"redacted.pdf", "redacted.txt", "receipt.json"} {
		entry, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		require.NoError(t, err)
		_, err = entry.Write([][]byte{pdf, text, encoded}[i])
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	require.NoError(t, os.WriteFile(f.args[5], buffer.Bytes(), 0600))
	return buffer.Bytes()
}

func redactionFixtureDigest(data []byte) processing.RedactionFileDigest {
	return processing.RedactionFileDigest{
		Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)),
	}
}

// Generate from the reviewed runs without any original PDF or page runtime.
// The malicious variant deliberately bypasses WriteFresh's mask binding using
// the rendition writer, then hashes the resulting well-formed PDF honestly.
func redactionVerifyPDF(t *testing.T, review processing.RedactionReview, presentation []pdfproduction.PagePresentation, scenario string) []byte {
	t.Helper()
	page := review.Resolved.Pages[0]
	pixels := image.NewNRGBA(image.Rect(0, 0, 2550, 3300))
	draw.Draw(pixels, pixels.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	masks, err := pdfproduction.PlanPixels(page, review.Resolved.RedactBoxes, review.Recipe)
	require.NoError(t, err)
	for _, mask := range masks {
		draw.Draw(pixels, mask, image.NewUniform(color.Black), image.Point{}, draw.Src)
	}
	layout := redaction.PageLayout{Source: page, Output: page}
	endorsements := []redaction.Endorsement{}
	if presentation != nil {
		p := presentation[0]
		layout, endorsements = p.Layout, p.Endorsements
		visible := endorsements
		if scenario == "wrong visible number" {
			visible = slices.Clone(endorsements)
			visible[0].Text = "Q000002"
		}
		raster, err := pdfproduction.Burn(pdfproduction.Raster{Pixels: pixels, Page: page, DPI: review.Recipe.DPI}, review.Resolved.RedactBoxes, review.Recipe)
		require.NoError(t, err)
		raster, err = pdfproduction.Endorse(raster, layout, visible, review.Recipe)
		require.NoError(t, err)
		pixels = raster.Pixels
	}
	if scenario == "unmasked PDF" {
		pixels.SetNRGBA(masks[0].Min.X+1, masks[0].Min.Y+1, color.NRGBA{R: 255, A: 255})
	}
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, pixels))
	data := encoded.Bytes()
	layoutJSON, err := canonical.Marshal(layout)
	require.NoError(t, err)
	endorsementsJSON, err := canonical.Marshal(endorsements)
	require.NoError(t, err)
	a := pdfproduction.PageArtifact{Page: layout.Output, Layout: layout, LayoutSHA256: redactionFixtureDigest(layoutJSON).SHA256,
		ResolvedSHA256: review.Resolved.SHA256, Runs: review.Resolved.Runs, Endorsements: endorsements,
		EndorsementsSHA256: redactionFixtureDigest(endorsementsJSON).SHA256,
		PNGSHA256:          redactionFixtureDigest(data).SHA256, PNGSize: int64(len(data)),
		OpenPNG: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}
	var out bytes.Buffer
	options := pdfproduction.PDFWriteOptions{Recipe: review.Recipe, StagingDirectory: t.TempDir(), MaxBytes: review.Recipe.MaxStagingBytes}
	sequence := &redactionPageSequence{artifact: a}
	if scenario == "unmasked PDF" || scenario == "wrong visible number" {
		err = pdfproduction.WriteRendition(t.Context(), &out, sequence, options)
	} else {
		err = pdfproduction.WriteFresh(t.Context(), &out, sequence, review.Resolved, options)
	}
	require.NoError(t, err)
	return out.Bytes()
}
