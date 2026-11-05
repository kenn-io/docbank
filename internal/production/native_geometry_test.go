package production

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
)

type geometryPages struct {
	values []pdfproduction.PageArtifact
	index  int
}

func (p *geometryPages) Next(context.Context) (pdfproduction.PageArtifact, error) {
	if p.index == len(p.values) {
		return pdfproduction.PageArtifact{}, io.EOF
	}
	result := p.values[p.index]
	p.index++
	return result, nil
}

func TestNativeGeometryPipelineControlIdentities(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	data, err := os.ReadFile("testdata/native-geometry-control.pdf")
	require.NoError(t, err)
	source := document.PageSource{
		VersionID: "00000000-0000-4000-8000-000000000001",
		SHA256:    "b893b95a5aae351d7447d7f662826eaff664fc8738d04125df25590a92d3aeaf",
		Size:      1618,
	}
	inspection, err := runtime.Inspect(t.Context(), data, source, "application/pdf")
	require.NoError(t, err)
	prep, err := PrepareNativePDF(t.Context(), NativePreparationInput{
		PDF: pdfproduction.Source{Reader: bytes.NewReader(data), SHA256: source.SHA256,
			Size: source.Size, StagingDirectory: t.TempDir()},
		Inspection: inspection,
		// Tool binary hashes vary by deployment; hold this input fixed to pin the observations.
		PageRuntimeFingerprint: strings.Repeat("b", 64),
	})
	require.NoError(t, err)
	// Captured with the pre-projection admission checks at 68c35c195. This synthetic
	// four-rotation crop was already accepted; its stored geometry must not drift.
	wantFrames := []string{
		"6d1e1fd88ba70cf18c71a9615cd7f1d2565b9008e74a1ee950a4c59385839809",
		"8682720c1d45223310f8e0ab480d9561622b3e8191860d7cd344e83c10af2082",
		"9b1296010714b3007fce478106a393e37fe6f03ea7d3db5bbfa157048232150a",
		"61aebfd6ac81395f388a5d77b59157918d0d7c50ee779122de5c839f5ba7b4dd",
	}
	require.Len(t, prep.Frames, len(wantFrames))
	for i, frame := range prep.Frames {
		_, digest, err := document.MarshalPageFrameV1(frame)
		require.NoError(t, err)
		require.Equal(t, wantFrames[i], digest, "page %d", i+1)
	}
	require.Equal(t, "92084e9a003f9fd40cd95946a0d38c7dd2d9e80a90dd9a76bac0feee0e1a7cde", prep.EvidenceSHA256)
	require.Equal(t, "acc947c72f05d58881077b76c04494e5ad46ebccfe91b8048b36cb4d682790ee", prep.Map.SHA256)
}

func TestNativeGeometryPipeline(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	for index, fixture := range redactiontest.GeometryFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			data := redactiontest.GeometryPDF(fixture.Media, fixture.Crop, []int{0, 90, 180, 270}, false)
			source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: sum(data), Size: int64(len(data))}
			inspection, err := runtime.Inspect(t.Context(), data, source, "application/pdf")
			require.NoError(t, err)
			input := pdfproduction.Source{Reader: bytes.NewReader(data), SHA256: source.SHA256, Size: source.Size, StagingDirectory: t.TempDir()}
			prep, err := PrepareNativePDF(t.Context(), NativePreparationInput{PDF: input, Inspection: inspection, PageRuntimeFingerprint: runtime.Fingerprint()})
			require.NoError(t, err)
			require.Len(t, prep.Map.Atoms, 40)
			require.Empty(t, prep.Map.Gaps)
			for i, frame := range prep.Frames {
				width, height := fixture.Physical[0], fixture.Physical[1]
				if i%2 != 0 {
					width, height = height, width
				}
				require.Equal(t, width, frame.Width)
				require.Equal(t, height, frame.Height)
			}
			recipe := pdfproduction.QualifiedRecipe()
			decisions := []redaction.Decision{}
			for i, page := range prep.Map.Pages {
				require.True(t, strings.HasPrefix(prep.Map.Text[page.Span.Start:page.Span.End], "SECRET KEEP"))
				decisions = append(decisions, redaction.Decision{
					ID:       fmt.Sprintf("00000000-0000-4000-8000-%012d", i+10),
					MemberID: "00000000-0000-4000-8000-000000000002", Action: "redact",
					Selector: redaction.Selector{Kind: "text", MapSHA256: prep.Map.SHA256,
						Span: &redaction.Span{Start: page.Span.Start, End: page.Span.Start + 6}},
				})
			}
			plan, err := redaction.Resolve(prep.Map, "redact_selected", decisions, recipe)
			require.NoError(t, err)
			text, err := redaction.Text(plan)
			require.NoError(t, err)
			require.Equal(t, strings.Repeat("[REDACTED] KEEP\f", 4), string(text))
			engine, err := pdfproduction.NewPDFium(recipe)
			require.NoError(t, err)
			defer func() { require.NoError(t, engine.Close()) }()
			artifacts := []pdfproduction.PageArtifact{}
			for _, page := range plan.Pages {
				raster, err := engine.Render(t.Context(), input, page, recipe)
				require.NoError(t, err)
				boxes := []redaction.Box{}
				for _, b := range plan.RedactBoxes {
					if b.Page == page.Number {
						boxes = append(boxes, b)
					}
				}
				burned, err := pdfproduction.Burn(raster, boxes, recipe)
				require.NoError(t, err)
				// These source-pixel windows are independent of map atoms and resolved masks.
				// They enclose Helvetica SECRET and KEEP at (72,72), rotated with the page.
				regions := [][4]image.Rectangle{
					{image.Rect(200, 160, 404, 201), image.Rect(199, 200, 240, 404), image.Rect(296, 199, 500, 240), image.Rect(160, 296, 201, 500)},
					{image.Rect(139, 3067, 343, 3106), image.Rect(194, 139, 233, 343), image.Rect(4457, 194, 4661, 233), image.Rect(3067, 4457, 3106, 4661)},
				}
				keepRegions := [][4]image.Rectangle{
					{image.Rect(416, 160, 551, 201), image.Rect(199, 416, 240, 551), image.Rect(149, 199, 284, 240), image.Rect(160, 149, 201, 284)},
					{image.Rect(356, 3067, 490, 3106), image.Rect(194, 356, 233, 490), image.Rect(4310, 194, 4444, 233), image.Rect(3067, 4310, 3106, 4444)},
				}
				w, h := fixture.Pixels[0], fixture.Pixels[1]
				if page.Number%2 == 0 {
					w, h = h, w
				}
				require.Equal(t, image.Rect(0, 0, w, h), raster.Pixels.Bounds())
				secretInk, keepInk, changed := 0, 0, 0
				secret, keep := regions[index][page.Number-1], keepRegions[index][page.Number-1]
				for y := secret.Min.Y; y < secret.Max.Y; y++ {
					for x := secret.Min.X; x < secret.Max.X; x++ {
						before, after := raster.Pixels.NRGBAAt(x, y), burned.Pixels.NRGBAAt(x, y)
						if before.R < 250 {
							secretInk++
							require.Equal(t, color.NRGBA{A: 255}, after)
						}
						if before != after {
							changed++
						}
					}
				}
				for y := keep.Min.Y; y < keep.Max.Y; y++ {
					for x := keep.Min.X; x < keep.Max.X; x++ {
						before := raster.Pixels.NRGBAAt(x, y)
						require.Equal(t, before, burned.Pixels.NRGBAAt(x, y))
						if before.R < 250 {
							keepInk++
						}
					}
				}
				require.Positive(t, secretInk)
				require.Positive(t, keepInk)
				require.Positive(t, changed)
				var encoded bytes.Buffer
				require.NoError(t, png.Encode(&encoded, burned.Pixels))
				pngData := encoded.Bytes()
				layout := redaction.PageLayout{Source: page, Output: page}
				lb, err := canonical.Marshal(layout)
				require.NoError(t, err)
				endorsements := []redaction.Endorsement{}
				eb, err := canonical.Marshal(endorsements)
				require.NoError(t, err)
				runs := []redaction.Run{}
				for _, run := range plan.Runs {
					if run.Page == page.Number {
						runs = append(runs, run)
					}
				}
				artifacts = append(artifacts, pdfproduction.PageArtifact{
					Page: page, Layout: layout, LayoutSHA256: sum(lb),
					Endorsements: endorsements, EndorsementsSHA256: sum(eb), Runs: runs,
					ResolvedSHA256: plan.SHA256, PNGSHA256: sum(pngData), PNGSize: int64(len(pngData)),
					OpenPNG: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(pngData)), nil },
				})
				raster.Pixels = nil
				burned.Pixels = nil
				debug.FreeOSMemory()
			}
			require.NoError(t, engine.Close())
			var output bytes.Buffer
			require.NoError(t, pdfproduction.WriteFresh(t.Context(), &output, &geometryPages{values: artifacts}, plan, pdfproduction.PDFWriteOptions{Recipe: recipe, StagingDirectory: t.TempDir(), MaxBytes: 512 << 20}))
			inputVerify := pdfproduction.VerificationInput{Pages: plan.Pages, NextPageArtifact: (&geometryPages{values: artifacts}).Next, StagingDirectory: t.TempDir()}
			for _, a := range artifacts {
				inputVerify.ResolvedSHA256 = append(inputVerify.ResolvedSHA256, a.ResolvedSHA256)
				inputVerify.LayoutSHA256 = append(inputVerify.LayoutSHA256, a.LayoutSHA256)
				inputVerify.EndorsementsSHA256 = append(inputVerify.EndorsementsSHA256, a.EndorsementsSHA256)
			}
			nextText := 0
			inputVerify.NextText = func(context.Context) (int, []byte, error) {
				if nextText == len(artifacts) {
					return 0, nil, io.EOF
				}
				a := artifacts[nextText]
				nextText++
				var b strings.Builder
				for _, run := range a.Runs {
					b.WriteString(run.Text)
				}
				return a.Page.Number, []byte(b.String()), nil
			}
			nextLayout := 0
			inputVerify.NextEndorsements = func(context.Context) (redaction.PageLayout, []redaction.Endorsement, error) {
				if nextLayout == len(artifacts) {
					return redaction.PageLayout{}, nil, io.EOF
				}
				a := artifacts[nextLayout]
				nextLayout++
				return a.Layout, a.Endorsements, nil
			}
			require.NoError(t, pdfproduction.VerifyFreshWithRecipe(t.Context(), bytes.NewReader(output.Bytes()), int64(output.Len()), inputVerify, recipe))
			require.NoError(t, pdfproduction.VerifySaved(t.Context(), bytes.NewReader(output.Bytes()), int64(output.Len()), plan, recipe, nil))
			// Independent Poppler text extraction checks the delivered PDF, not the sidecar.
			textTool, err := exec.LookPath("pdftotext")
			if err != nil {
				textTool = filepath.Join(filepath.Dir(os.Getenv("DOCBANK_TEST_PDFTOPPM")), "pdftotext")
			}
			file := filepath.Join(t.TempDir(), "redacted.pdf")
			require.NoError(t, os.WriteFile(file, output.Bytes(), 0600))
			//nolint:gosec // Poppler from PATH or the operator-selected test bundle; arguments are fixed/private.
			extracted, err := exec.CommandContext(t.Context(), textTool, "-raw", file, "-").CombinedOutput()
			require.NoError(t, err, string(extracted))
			require.Equal(t, strings.Fields(strings.Repeat("[REDACTED] KEEP ", 4)), strings.Fields(string(extracted)))
		})
	}
}
