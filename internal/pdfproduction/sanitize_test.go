package pdfproduction

import (
	"bytes"
	"context"
	"image"
	"image/draw"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pdf/fpdf"
	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

func sanitizedFixture(t *testing.T, text string) (redaction.Resolved, PageArtifact, redaction.Recipe) {
	t.Helper()
	r, recipe := whiteRaster(t)
	r.Page.Span.End = int64(len(text))
	box := redaction.Box{Page: 1, FrameSHA256: r.Page.FrameSHA256, X0: 1000, Y0: 1000, X1: 9000, Y1: 3000}
	runs := []redaction.Run{{Kind: "text", Text: text, Page: 1, SourceSpan: &redaction.Span{End: int64(len(text))}, Boxes: []redaction.Box{box}, FontSizeMilliPoints: 8000}}
	if strings.TrimSpace(text) == "" {
		runs[0].Boxes = []redaction.Box{}
	}
	recipeBytes, err := canonical.Marshal(recipe)
	require.NoError(t, err)
	plan := redaction.Resolved{Contract: "redaction-plan/v1", MapSHA256: digest([]byte("synthetic map")), RecipeSHA256: digest(recipeBytes), PageCount: 1, Pages: []redaction.Page{r.Page}, Gaps: []redaction.Gap{}, RedactBoxes: []redaction.Box{}, Removed: []redaction.Span{}, Runs: runs, UncertainDecisionIDs: []string{}, Regions: []redaction.RedactionRegion{}}
	sealPlan(t, &plan)
	l := redaction.PageLayout{Source: r.Page, Output: r.Page}
	layoutBytes, err := canonical.Marshal(l)
	require.NoError(t, err)
	a := PageArtifact{Page: r.Page, ResolvedSHA256: plan.SHA256, Runs: runs, Layout: l, LayoutSHA256: digest(layoutBytes), Endorsements: []redaction.Endorsement{}, EndorsementsSHA256: digest([]byte("[]"))}
	encodeArtifact(t, &a, r.Pixels)
	return plan, a, recipe
}

func TestSanitizedNativeAndScanThreeTargetsEndorsementsAndIndependentOCR(t *testing.T) {
	for _, scan := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "scan"}[scan], func(t *testing.T) {
			const text = "SECRET keep SECRET keep SECRET keep SECRET"
			const canary = "PRIVATE-METADATA-ATTACHMENT-ACTUALTEXT-CANARY"
			source := fpdf.New("P", "pt", "Letter", "")
			source.SetCompression(false)
			source.SetSubject(canary, false)
			source.SetAttachments([]fpdf.Attachment{{Content: []byte(canary), Filename: "private-source.txt"}})
			source.AddPage()
			source.SetFont("Helvetica", "", 14)
			source.RawWriteStr("/Span << /ActualText (" + canary + ") >> BDC")
			for i, word := range []string{"SECRET", "keep", "SECRET", "keep", "SECRET", "keep", "SECRET"} {
				source.Text(float64(36+i*80), 72, word)
			}
			source.RawWriteStr("EMC")
			source.SetDrawColor(255, 0, 0)
			source.Line(30, 150, 580, 150)
			var original bytes.Buffer
			require.NoError(t, source.Output(&original))
			require.Contains(t, original.String(), canary)
			p, a, r := sanitizedFixture(t, text)
			page := a.Page
			page.Width = 85000
			page.Height = 110000
			p.Pages = []redaction.Page{page}
			a.Page = page
			a.Layout = redaction.PageLayout{Source: page, Output: page}
			p.RedactBoxes = []redaction.Box{}
			for _, x := range []int64{4000, 26000, 48000} {
				p.RedactBoxes = append(p.RedactBoxes, redaction.Box{Page: 1, FrameSHA256: page.FrameSHA256, X0: x, Y0: 7800, X1: x + 11000, Y1: 10500})
			}
			p.Removed = []redaction.Span{{Start: 0, End: 6}, {Start: 12, End: 18}, {Start: 24, End: 30}}
			p.Runs = []redaction.Run{}
			for i, span := range []redaction.Span{{Start: 6, End: 12}, {Start: 18, End: 24}, {Start: 30, End: 42}} {
				p.Runs = append(p.Runs, redaction.Run{Kind: "redaction", Text: "[REDACTED]", Page: 1, Anchor: span.Start - 6, Boxes: []redaction.Box{p.RedactBoxes[i]}, FontSizeMilliPoints: 8000})
				x := []int64{16000, 38000, 60000}[i]
				boxes := []redaction.Box{{Page: 1, FrameSHA256: page.FrameSHA256, X0: x, Y0: 7800, X1: x + 7000, Y1: 10500}}
				if i == 2 {
					boxes = append(boxes, redaction.Box{Page: 1, FrameSHA256: page.FrameSHA256, X0: 71000, Y0: 7800, X1: 81000, Y1: 10500})
				}
				p.Runs = append(p.Runs, redaction.Run{Kind: "text", Text: text[span.Start:span.End], Page: 1, Anchor: span.Start, SourceSpan: &span, Boxes: boxes, FontSizeMilliPoints: 8000})
			}
			sealPlan(t, &p)
			a.ResolvedSHA256 = p.SHA256
			a.Runs = p.Runs
			engine, err := NewPDFium(r)
			require.NoError(t, err)
			defer func() { require.NoError(t, engine.Close()) }()
			raster, err := engine.Render(t.Context(), Source{Reader: bytes.NewReader(original.Bytes()), Size: int64(original.Len()), SHA256: digest(original.Bytes())}, page, r)
			require.NoError(t, err)
			if scan {
				scanArtifact := a
				scanArtifact.Runs = nil
				scanArtifact.LayoutSHA256 = mustDigest(t, scanArtifact.Layout)
				encodeArtifact(t, &scanArtifact, raster.Pixels)
				var wrapper bytes.Buffer
				require.NoError(t, WriteRendition(t.Context(), &wrapper, &artifactSequence{pages: []PageArtifact{scanArtifact}}, r))
				raster, err = engine.Render(t.Context(), Source{Reader: bytes.NewReader(wrapper.Bytes()), Size: int64(wrapper.Len()), SHA256: digest(wrapper.Bytes())}, page, r)
				require.NoError(t, err)
			}
			require.NoError(t, engine.Close())
			burned, err := Burn(raster, p.RedactBoxes, r)
			require.NoError(t, err)
			output := page
			output.Height += 5000
			output.FrameSHA256 = digest([]byte("final numbered frame"))
			a.Page = output
			a.Layout = redaction.PageLayout{Source: page, Output: output, StripHeight: 5000}
			a.LayoutSHA256 = mustDigest(t, a.Layout)
			label := p.RedactBoxes[0]
			label.FrameSHA256 = output.FrameSHA256
			a.Endorsements = []redaction.Endorsement{{Kind: "label", Text: "PUBLIC", FontSHA256: fontSHA256, FontSizeMilliPoints: 8000, Color: "#ffffff", Box: label}, {Kind: "number", Text: "ABC-000001", FontSHA256: fontSHA256, FontSizeMilliPoints: 10000, Color: "#000000", Box: redaction.Box{Page: 1, FrameSHA256: output.FrameSHA256, X0: 1000, Y0: 111000, X1: 30000, Y1: 114500}}}
			a.EndorsementsSHA256 = mustDigest(t, a.Endorsements)
			final, err := Endorse(burned, a.Layout, a.Endorsements, r)
			require.NoError(t, err)
			encodeArtifact(t, &a, final.Pixels)
			var pdf bytes.Buffer
			require.NoError(t, WriteFresh(t.Context(), &pdf, &artifactSequence{pages: []PageArtifact{a}}, p, r))
			require.NoError(t, VerifyFreshWithRecipe(t.Context(), bytes.NewReader(pdf.Bytes()), int64(pdf.Len()), verificationFor([]PageArtifact{a}), r))
			extracted := extractIndependent(t, pdf.Bytes())
			require.Equal(t, 1, strings.Count(extracted, "SECRET"))
			require.Equal(t, 3, strings.Count(extracted, "keep"))
			require.Equal(t, 1, strings.Count(extracted, "PUBLIC"))
			require.Equal(t, 1, strings.Count(extracted, "ABC-000001"))
			require.NotContains(t, extracted, canary)
			txt, err := redaction.Text(p)
			require.NoError(t, err)
			require.Equal(t, "[REDACTED] keep [REDACTED] keep [REDACTED] keep SECRET\f", string(txt))
			for _, private := range []string{canary, "private-source.txt", "/ActualText", "/EmbeddedFile", "/Metadata", p.SHA256, a.LayoutSHA256, a.EndorsementsSHA256} {
				require.NotContains(t, pdf.String(), private)
			}
			writeTaskBEvidence(t, map[bool]string{false: "native-sanitized.pdf", true: "scan-sanitized.pdf"}[scan], pdf.Bytes())
			t.Run("optional_poppler_and_ocr", func(t *testing.T) {
				for _, tool := range []string{"pdftotext", "pdftoppm", "tesseract"} {
					if _, err := exec.LookPath(tool); err != nil {
						t.Skip("optional independent development tool unavailable: " + tool)
					}
				}
				dir := t.TempDir()
				file := filepath.Join(dir, "synthetic.pdf")
				require.NoError(t, os.WriteFile(file, pdf.Bytes(), 0600))
				extracted, err := exec.CommandContext(t.Context(), "pdftotext", "-raw", file, "-").CombinedOutput()
				require.NoError(t, err, string(extracted))
				require.Equal(t, 1, strings.Count(string(extracted), "SECRET"))
				require.Equal(t, 1, strings.Count(string(extracted), "ABC-000001"))
				prefix := filepath.Join(dir, "page")
				log, err := exec.CommandContext(t.Context(), "pdftoppm", "-singlefile", "-r", "300", "-png", file, prefix).CombinedOutput()
				require.NoError(t, err, string(log))
				log, err = exec.CommandContext(t.Context(), "tesseract", prefix+".png", "stdout", "--psm", "11").CombinedOutput()
				require.NoError(t, err, string(log))
				require.Equal(t, 1, strings.Count(string(log), "SECRET"), string(log))
				require.Contains(t, string(log), "ABC-000001")
				require.NotContains(t, string(log), canary)
				// Sparse-page OCR treats adjacent black masks as layout noise.
				// Inspect each retained location in the independently rendered page.
				pageFile, err := os.Open(prefix + ".png")
				require.NoError(t, err)
				rendered, err := png.Decode(pageFile)
				require.NoError(t, err)
				require.NoError(t, pageFile.Close())
				for _, x := range []int{470, 1135, 1800} {
					crop := image.NewNRGBA(image.Rect(0, 0, 210, 110))
					draw.Draw(crop, crop.Bounds(), rendered, image.Pt(x, 220), draw.Src)
					var data bytes.Buffer
					require.NoError(t, png.Encode(&data, crop))
					cropPath := filepath.Join(dir, "retained.png")
					require.NoError(t, os.WriteFile(cropPath, data.Bytes(), 0600))
					log, err := exec.CommandContext(t.Context(), "tesseract", cropPath, "stdout", "--psm", "7").CombinedOutput()
					require.NoError(t, err, string(log))
					require.Equal(t, "keep", strings.TrimSpace(string(log)))
				}
			})
		})
	}
}

func mustDigest(t *testing.T, v any) string {
	t.Helper()
	b, err := canonical.Marshal(v)
	require.NoError(t, err)
	return digest(b)
}

func sealPlan(t *testing.T, p *redaction.Resolved) {
	t.Helper()
	_, sha, err := redaction.CanonicalResolved(*p)
	require.NoError(t, err)
	p.SHA256 = sha
}
func encodeArtifact(t *testing.T, a *PageArtifact, p image.Image) {
	t.Helper()
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, p))
	b := bytes.Clone(data.Bytes())
	a.PNGSHA256 = digest(b)
	a.PNGSize = int64(len(b))
	a.OpenPNG = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}

func TestSanitizedWriterRequiresPlanBeforePageWork(t *testing.T) {
	for name, mutate := range map[string]func(*redaction.Resolved){"self digest": func(p *redaction.Resolved) { p.Runs[0].Text = "evil evil" }, "recipe": func(p *redaction.Resolved) { p.RecipeSHA256 = digest([]byte("other recipe")); sealPlan(t, p) }} {
		t.Run(name, func(t *testing.T) {
			p, a, r := sanitizedFixture(t, "keep keep")
			mutate(&p)
			seq := &artifactSequence{pages: []PageArtifact{a}}
			var out bytes.Buffer
			require.Error(t, WriteFresh(t.Context(), &out, seq, p, r))
			require.Zero(t, seq.index)
			require.Empty(t, out.Bytes())
		})
	}
}

func TestSanitizedWriterRejectsOversizedPageTextBeforeConsumingPages(t *testing.T) {
	p, a, r := sanitizedFixture(t, strings.Repeat("x", maxTextBytes+1))
	seq := &artifactSequence{pages: []PageArtifact{a}}
	var out bytes.Buffer
	require.Error(t, WriteFresh(t.Context(), &out, seq, p, r))
	require.Zero(t, seq.index)
}

func TestSanitizedWriterRejectsRunSubstitutionAndMutatedMaskPixels(t *testing.T) {
	p, a, r := sanitizedFixture(t, "keep keep")
	a.Runs = append([]redaction.Run(nil), a.Runs...)
	a.Runs[0].Text = "evil evil"
	var out bytes.Buffer
	require.Error(t, WriteFresh(t.Context(), &out, &artifactSequence{pages: []PageArtifact{a}}, p, r))
	require.Empty(t, out.Bytes())
	p, a, r = sanitizedFixture(t, "secret")
	mask := p.Runs[0].Boxes[0]
	p.RedactBoxes = []redaction.Box{mask}
	p.Removed = []redaction.Span{{End: 6}}
	p.Runs = []redaction.Run{{Kind: "redaction", Text: "[REDACTED]", Page: 1, Boxes: []redaction.Box{mask}, FontSizeMilliPoints: 8000}}
	sealPlan(t, &p)
	a.Runs = p.Runs
	a.ResolvedSHA256 = p.SHA256
	require.Error(t, WriteFresh(t.Context(), &out, &artifactSequence{pages: []PageArtifact{a}}, p, r))
	require.Empty(t, out.Bytes())
	raster, _ := whiteRaster(t)
	raster.Page = a.Layout.Source
	burned, err := Burn(raster, p.RedactBoxes, r)
	require.NoError(t, err)
	encodeArtifact(t, &a, burned.Pixels)
	require.NoError(t, WriteFresh(t.Context(), &out, &artifactSequence{pages: []PageArtifact{a}}, p, r))
}

func TestSanitizedWriterMultiBoxSingleOccurrenceAndPrivateHashes(t *testing.T) {
	p, a, r := sanitizedFixture(t, "keep keep")
	b := p.Runs[0].Boxes[0]
	b.Y0 = 4000
	b.Y1 = 6000
	p.Runs[0].Boxes = append(p.Runs[0].Boxes, b)
	sealPlan(t, &p)
	a.Runs = p.Runs
	a.ResolvedSHA256 = p.SHA256
	var out bytes.Buffer
	require.NoError(t, WriteFresh(t.Context(), &out, &artifactSequence{pages: []PageArtifact{a}}, p, r))
	require.NotContains(t, out.String(), p.SHA256)
	require.NotContains(t, out.String(), a.LayoutSHA256)
	require.NotContains(t, out.String(), "/DocbankResolved")
	require.NoError(t, VerifyFreshWithRecipe(t.Context(), bytes.NewReader(out.Bytes()), int64(out.Len()), verificationFor([]PageArtifact{a}), r))
}

func TestSanitizedWriterWhitespaceUnicodeAnd600DPI(t *testing.T) {
	for _, example := range []struct{ text, extracted string }{
		{"  leading middle  trailing  ", " leading middle trailing "},
		{"paragraph one\n\nparagraph two\n", "paragraph one\n\nparagraph two\n"},
		{" \t\n ", " \t\n "}, {"café 給与", "café 給与"},
		{"𠮷 retained", "𠮷 retained"},
	} {
		text := example.text
		t.Run(text, func(t *testing.T) {
			p, a, r := sanitizedFixture(t, text)
			var out bytes.Buffer
			require.NoError(t, WriteFresh(t.Context(), &out, &artifactSequence{pages: []PageArtifact{a}}, p, r))
			txt, err := redaction.Text(p)
			require.NoError(t, err)
			require.Equal(t, text+"\f", string(txt))
			require.Equal(t, example.extracted, extractIndependent(t, out.Bytes()))
			require.NoError(t, VerifyFreshWithRecipe(t.Context(), bytes.NewReader(out.Bytes()), int64(out.Len()), verificationFor([]PageArtifact{a}), r))
		})
	}
	p, a, _ := sanitizedFixture(t, "keep")
	r, err := QualifiedRecipeForDPI(600)
	require.NoError(t, err)
	b, err := canonical.Marshal(r)
	require.NoError(t, err)
	p.RecipeSHA256 = digest(b)
	sealPlan(t, &p)
	a.ResolvedSHA256 = p.SHA256
	raster, _ := whiteRaster(t)
	raster.Pixels = image.NewNRGBA(image.Rect(0, 0, 600, 600))
	raster.Page = a.Page
	raster.DPI = 600
	raster, err = Burn(raster, nil, r)
	require.NoError(t, err)
	encodeArtifact(t, &a, raster.Pixels)
	var out bytes.Buffer
	require.NoError(t, WriteFresh(t.Context(), &out, &artifactSequence{pages: []PageArtifact{a}}, p, r))
	require.NoError(t, VerifyFreshWithRecipe(t.Context(), bytes.NewReader(out.Bytes()), int64(out.Len()), verificationFor([]PageArtifact{a}), r))
}

func extractIndependent(t *testing.T, data []byte) string {
	t.Helper()
	engine, err := NewPDFium(QualifiedRecipe())
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	var result string
	concrete, ok := engine.(*pdfiumEngine)
	require.True(t, ok)
	err = concrete.withInstanceContext(t.Context(), func(_ context.Context, instance pdfium.Pdfium) error {
		doc, err := instance.OpenDocument(&requests.OpenDocument{File: &data})
		require.NoError(t, err)
		page := requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: 0}}
		loaded, err := instance.FPDFText_LoadPage(&requests.FPDFText_LoadPage{Page: page})
		require.NoError(t, err)
		defer func() {
			_, closeErr := instance.FPDFText_ClosePage(&requests.FPDFText_ClosePage{TextPage: loaded.TextPage})
			require.NoError(t, closeErr)
		}()
		count, err := instance.FPDFText_CountChars(&requests.FPDFText_CountChars{TextPage: loaded.TextPage})
		require.NoError(t, err)
		text, err := instance.FPDFText_GetText(&requests.FPDFText_GetText{TextPage: loaded.TextPage, Count: count.Count})
		require.NoError(t, err)
		result = text.Text
		return nil
	})
	require.NoError(t, err)
	return result
}
