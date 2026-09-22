package production

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/png"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestMapCannotSplitUTF8(t *testing.T) {
	m := redactiontest.Map("abc")
	m.Text = "éx"
	m.Atoms[0].Span = redaction.Span{Start: 0, End: 1}
	require.Error(t, redaction.ValidateMap(m))
}

func TestAlignRepeatedNativeTextKeepsDistinctPositions(t *testing.T) {
	pdf := redactiontest.PDF(t, []string{"same same"}, "alignment")
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: sum(pdf), Size: int64(len(pdf))}
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
	require.NoError(t, err)
	evidence := evidenceFor(t, "same same")

	m, err := Align(t.Context(), AlignmentInput{PDF: pdfproduction.Source{Reader: bytes.NewReader(pdf), Size: int64(len(pdf)), SHA256: sum(pdf)}, Pages: []document.PageFrameV1{frame}, Evidence: evidence, EvidenceSHA256: evidence.Checksum})
	require.NoError(t, err)
	require.Equal(t, "same same", strings.TrimSpace(m.Text))
	require.GreaterOrEqual(t, len(m.Atoms), 8)
	require.NotEqual(t, m.Atoms[0].Boxes[0], m.Atoms[5].Boxes[0])
	require.NoError(t, redaction.ValidateMap(m))
}

func TestSemanticUnitsUseRegionPositionForSecondRepeatedOccurrence(t *testing.T) {
	m := redactiontest.Map("same same")
	policy, err := document.NewEvidencePolicy(1 << 20)
	require.NoError(t, err)
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete, Family: "pdf", UnitKind: document.EvidenceUnitPage, Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: "same", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}, Regions: []document.SourceEvidenceRegionV1{{ProviderID: "second-occurrence", Kind: document.EvidenceRegionParagraph, Order: 0, TextRange: document.EvidenceTextRangeV1{Start: 0, End: 4}, Geometry: &document.SourceEvidenceGeometryV1{Boxes: []document.EvidenceBoxV1{{Left: 560000, Top: 0, Right: 1000000, Bottom: 10000}}, CoordinateOrigin: document.EvidenceCoordinateTopLeft, CoordinateSpace: document.EvidenceCoordinatePage, Width: 1000000, Height: 1000000, Scale: 1000000, Unit: document.EvidenceGeometryNormalized}}}}}}, policy)
	require.NoError(t, err)

	units, err := semanticUnits(m, []document.PageFrameV1{{}}, evidence, evidence.Checksum)
	require.NoError(t, err)
	require.Equal(t, []redaction.Span{{Start: 5, End: 9}}, units[0].Spans)
}

func TestNativeMultiColumnContentOrderUsesDistinctPositions(t *testing.T) {
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	pdf.Text(350, 72, "right")
	pdf.Text(50, 72, "left")
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))
	data := encoded.Bytes()
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: sum(data), Size: int64(len(data))}
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
	require.NoError(t, err)
	inspected, err := pdfproduction.InspectNativeText(t.Context(), pdfproduction.Source{Reader: bytes.NewReader(data), Size: int64(len(data)), SHA256: sum(data)}, []document.PageFrameV1{frame})
	require.NoError(t, err)
	_, frameSHA, err := document.MarshalPageFrameV1(frame)
	require.NoError(t, err)
	page := redaction.Page{Number: 1, FrameSHA256: frameSHA, Width: frame.Width, Height: frame.Height}
	m, err := alignNative([]document.PageFrameV1{frame}, inspected, []redaction.Page{page}, strings.Repeat("e", 64), sum(data))
	require.NoError(t, err)
	require.Equal(t, "leftright", strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, m.Text))
	require.Less(t, m.Atoms[0].Boxes[0].X0, m.Atoms[4].Boxes[0].X0)
}

func TestAlignFailsClosedOnSourceAndFrameMismatch(t *testing.T) {
	pdf := redactiontest.PDF(t, []string{"visible"}, "alignment")
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: sum(pdf), Size: int64(len(pdf))}
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 90)
	require.NoError(t, err)
	evidence := evidenceFor(t, "visible")
	_, err = Align(t.Context(), AlignmentInput{PDF: pdfproduction.Source{Reader: bytes.NewReader(pdf), Size: int64(len(pdf)), SHA256: strings.Repeat("f", 64)}, Pages: []document.PageFrameV1{frame}, Evidence: evidence, EvidenceSHA256: evidence.Checksum})
	require.Error(t, err)
}

func TestAlignRejectsHiddenNativeTextAndRetainsExplicitGap(t *testing.T) {
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	pdf.SetTextRenderingMode(3)
	pdf.Text(72, 72, "SECRET")
	pdf.SetTextRenderingMode(0)
	pdf.Text(72, 100, "visible")
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))
	data := encoded.Bytes()
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: sum(data), Size: int64(len(data))}
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
	require.NoError(t, err)
	evidence := evidenceFor(t, "visible")
	m, err := Align(t.Context(), AlignmentInput{PDF: pdfproduction.Source{Reader: bytes.NewReader(data), Size: int64(len(data)), SHA256: sum(data)}, Pages: []document.PageFrameV1{frame}, Evidence: evidence, EvidenceSHA256: evidence.Checksum})
	require.NoError(t, err)
	require.NotContains(t, m.Text, "SECRET")
	require.NotEmpty(t, m.Gaps)
}

func TestAlignNativeDoesNotInventOrderForImageBetweenText(t *testing.T) {
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: strings.Repeat("a", 64), Size: 123}
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
	require.NoError(t, err)
	_, frameSHA, err := document.MarshalPageFrameV1(frame)
	require.NoError(t, err)
	page := redaction.Page{Number: 1, FrameSHA256: frameSHA, Width: frame.Width, Height: frame.Height}
	observed := pdfproduction.NativeTextPage{Number: 1,
		NonTextBounds: [][4]int64{{500000, 7000000, 600000, 7100000}},
		Glyphs: []pdfproduction.NativeGlyph{
			{Text: "A", Bounds: [4]int64{700000, 7000000, 800000, 7100000}},
			{Text: "B", Bounds: [4]int64{900000, 7000000, 1000000, 7100000}},
		}}
	m, err := alignNative([]document.PageFrameV1{frame}, []pdfproduction.NativeTextPage{observed}, []redaction.Page{page}, strings.Repeat("b", 64), strings.Repeat("c", 64))
	require.NoError(t, err)
	require.Equal(t, "AB", m.Text)
	require.Len(t, m.Gaps, 1)
	require.True(t, m.Gaps[0].Unordered)
	m = redaction.NormalizeTextMap(m)
	_, m.SHA256, err = redaction.CanonicalTextMap(m)
	require.NoError(t, err)
	mask := redaction.Decision{ID: "11111111-1111-4111-8111-111111111111", MemberID: "22222222-2222-4222-8222-222222222222", Action: "redact",
		Selector: redaction.Selector{Kind: "rectangle", MapSHA256: m.SHA256, Boxes: []redaction.Box{m.Gaps[0].Box}}}
	_, err = redaction.Resolve(m, "redact_selected", []redaction.Decision{mask}, pdfproduction.QualifiedRecipe())
	var problem *redaction.Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "mapping_incomplete", problem.Code)
	mask.Selector = redaction.Selector{Kind: "page", MapSHA256: m.SHA256, Pages: []int{1}}
	_, err = redaction.Resolve(m, "redact_selected", []redaction.Decision{mask}, pdfproduction.QualifiedRecipe())
	require.NoError(t, err)
}

func TestAlignRejectsNativeTextCoveredByLaterOpaquePaint(t *testing.T) {
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	pdf.Text(72, 72, "SECRET")
	pdf.SetFillColor(0, 0, 0)
	pdf.Rect(68, 58, 70, 18, "F")
	pdf.SetTextColor(0, 0, 0)
	pdf.Text(72, 100, "visible")
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))
	data := encoded.Bytes()
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: sum(data), Size: int64(len(data))}
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
	require.NoError(t, err)
	evidence := evidenceFor(t, "visible")

	m, err := Align(t.Context(), AlignmentInput{PDF: pdfproduction.Source{Reader: bytes.NewReader(data), Size: int64(len(data)), SHA256: sum(data)}, Pages: []document.PageFrameV1{frame}, Evidence: evidence, EvidenceSHA256: evidence.Checksum})
	require.NoError(t, err)
	require.NotContains(t, m.Text, "SECRET")
	require.Contains(t, m.Text, "visible")
	require.NotEmpty(t, m.Gaps)
}

func TestAlignRejectsNativeTextClippedOutOfFinalPixels(t *testing.T) {
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	pdf.RawWriteStr("q 0 0 1 1 re W n\n")
	pdf.Text(72, 72, "CLIPPED")
	pdf.RawWriteStr("Q\n")
	pdf.Text(72, 100, "visible")
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))
	m := alignNativeFixture(t, encoded.Bytes(), "visible")
	require.NotContains(t, m.Text, "CLIPPED")
	require.Contains(t, m.Text, "visible")
	require.NotEmpty(t, m.Gaps)
}

func TestAlignRejectsNativeTextCoveredByLaterImage(t *testing.T) {
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	pdf.Text(72, 72, "SECRET")
	cover := image.NewNRGBA(image.Rect(0, 0, 70, 18))
	for i := 0; i < len(cover.Pix); i += 4 {
		cover.Pix[i], cover.Pix[i+1], cover.Pix[i+2], cover.Pix[i+3] = 0, 0, 0, 255
	}
	options := fpdf.ImageOptions{ImageType: "PNG", ReadDpi: false}
	pdf.RegisterImageOptionsReader("cover", options, encodePNG(t, cover))
	pdf.ImageOptions("cover", 68, 58, 70, 18, false, options, 0, "")
	pdf.Text(72, 100, "visible")
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))
	m := alignNativeFixture(t, encoded.Bytes(), "visible")
	require.NotContains(t, m.Text, "SECRET")
	require.Contains(t, m.Text, "visible")
	require.NotEmpty(t, m.Gaps)
}

func TestAlignDoesNotLetOverlappingGlyphInSameObjectProveCoveredGlyphVisible(t *testing.T) {
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.SetFont("Helvetica", "", 20)
	pdf.AddPage()
	pdf.SetWordSpacing(-30)
	pdf.Text(72, 72, "A B")
	pdf.SetFillColor(0, 0, 0)
	pdf.Rect(70, 51, 17, 24, "F")
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))
	payload := encoded.Bytes()
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: sum(payload), Size: int64(len(payload))}
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
	require.NoError(t, err)
	evidence := evidenceFor(t, "B")
	m, err := Align(t.Context(), AlignmentInput{PDF: pdfproduction.Source{Reader: bytes.NewReader(payload), Size: int64(len(payload)), SHA256: sum(payload)},
		Pages: []document.PageFrameV1{frame}, Evidence: evidence, EvidenceSHA256: evidence.Checksum,
		Units: []redaction.Unit{{ID: "paragraph:b", Kind: "paragraph", Spans: []redaction.Span{{Start: 1, End: 2}}}}})
	require.NoError(t, err)
	require.NotContains(t, m.Text, "A")
	require.Contains(t, m.Text, "B")
	require.NotEmpty(t, m.Gaps)
}

func TestAlignNativeMixedRasterContentCreatesCoverageGap(t *testing.T) {
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	pdf.Text(72, 72, "native heading")
	raster := image.NewNRGBA(image.Rect(0, 0, 120, 24))
	for index := 0; index < len(raster.Pix); index += 4 {
		raster.Pix[index], raster.Pix[index+1], raster.Pix[index+2], raster.Pix[index+3] = 255, 255, 255, 255
	}
	options := fpdf.ImageOptions{ImageType: "PNG", ReadDpi: false}
	pdf.RegisterImageOptionsReader("raster-text", options, encodePNG(t, raster))
	pdf.ImageOptions("raster-text", 72, 100, 120, 24, false, options, 0, "")
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))

	m := alignNativeFixture(t, encoded.Bytes(), "native heading")
	require.Contains(t, m.Text, "native heading")
	require.NotEmpty(t, m.Gaps, "raster content must remain explicit incomplete text coverage")
}

func TestAlignNativeAnnotationCreatesCoverageGap(t *testing.T) {
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	pdf.Text(72, 72, "native")
	link := pdf.AddLink()
	pdf.SetLink(link, 0, 1)
	pdf.Link(70, 58, 80, 20, link)
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))

	m := alignNativeFixture(t, encoded.Bytes(), "native")
	require.Contains(t, m.Text, "native")
	require.NotEmpty(t, m.Gaps, "annotation appearance authority is outside page objects")
}

func TestAlignPreviouslyRedactedPDFCannotRecoverCoveredNativeText(t *testing.T) {
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	pdf.Text(72, 72, "PREVIOUSLY REDACTED")
	pdf.SetFillColor(0, 0, 0)
	pdf.Rect(68, 58, 150, 18, "F")
	pdf.Text(72, 100, "visible")
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))
	m := alignNativeFixture(t, encoded.Bytes(), "visible")
	require.NotContains(t, m.Text, "PREVIOUSLY")
	require.NotContains(t, m.Text, "REDACTED")
	require.Contains(t, m.Text, "visible")
}

func TestAlignRejectsActualTextSubstitutionAsVisibleAuthority(t *testing.T) {
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	pdf.RawWriteStr("/Span << /ActualText (different) >> BDC\n")
	pdf.Text(72, 72, "shown")
	pdf.RawWriteStr("EMC\n")
	pdf.Text(72, 100, "visible")
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))
	data := encoded.Bytes()
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: sum(data), Size: int64(len(data))}
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
	require.NoError(t, err)
	evidence := evidenceFor(t, "visible")
	m, err := Align(t.Context(), AlignmentInput{PDF: pdfproduction.Source{Reader: bytes.NewReader(data), Size: int64(len(data)), SHA256: sum(data)}, Pages: []document.PageFrameV1{frame}, Evidence: evidence, EvidenceSHA256: evidence.Checksum})
	require.NoError(t, err)
	require.NotContains(t, m.Text, "shown")
	require.NotContains(t, m.Text, "different")
	require.NotEmpty(t, m.Gaps)
}

func TestAlignCroppedRotatedFramesUseExactOutwardGeometry(t *testing.T) {
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: strings.Repeat("a", 64), Size: 123}
	raw := [4]int64{720000, 7000000, 1000000, 7200000}
	tests := []struct {
		rotation int
		want     [4]int64
	}{
		{0, [4]int64{0, 0, 3889, 2778}},
		{90, [4]int64{87222, 0, 90000, 3889}},
		{180, [4]int64{61111, 87222, 65000, 90000}},
		{270, [4]int64{0, 61111, 2778, 65000}},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("rotation-%d", test.rotation), func(t *testing.T) {
			frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 612, 792}, [4]float64{72, 72, 540, 720}, test.rotation)
			require.NoError(t, err)
			_, frameSHA, err := document.MarshalPageFrameV1(frame)
			require.NoError(t, err)
			box, ok, err := pdfproduction.PhysicalBox(frame, raw, redaction.Page{Number: 1, FrameSHA256: frameSHA, Width: frame.Width, Height: frame.Height})
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, test.want, [4]int64{box.X0, box.Y0, box.X1, box.Y1})
		})
	}
}

func TestAlignRejectsWrongRetainedEvidenceDigest(t *testing.T) {
	pdf := redactiontest.PDF(t, []string{"visible"}, "alignment")
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: sum(pdf), Size: int64(len(pdf))}
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
	require.NoError(t, err)
	evidence := evidenceFor(t, "visible")
	_, err = Align(t.Context(), AlignmentInput{PDF: pdfproduction.Source{Reader: bytes.NewReader(pdf), Size: int64(len(pdf)), SHA256: sum(pdf)}, Pages: []document.PageFrameV1{frame}, Evidence: evidence, EvidenceSHA256: strings.Repeat("f", 64)})
	var problem *Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "bound_evidence_mismatch", problem.Code)
}

func TestAlignScanRequiresTrustedRetainedPixelAuthority(t *testing.T) {
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.AddPage()
	pdf.SetFillColor(0, 0, 0)
	pdf.Rect(72, 72, 100, 20, "F")
	var encoded bytes.Buffer
	require.NoError(t, pdf.Output(&encoded))
	data := encoded.Bytes()
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: sum(data), Size: int64(len(data))}
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
	require.NoError(t, err)
	evidence := scanEvidenceFor(t, "scanned words")
	_, err = Align(t.Context(), AlignmentInput{PDF: pdfproduction.Source{Reader: bytes.NewReader(data), Size: int64(len(data)), SHA256: sum(data)}, Pages: []document.PageFrameV1{frame}, Evidence: evidence, EvidenceSHA256: evidence.Checksum})
	var problem *Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "mapping_incomplete", problem.Code)
}

func TestAlignEvidenceAssignsExactContiguousPageSpans(t *testing.T) {
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: strings.Repeat("a", 64), Size: 123}
	frames := make([]document.PageFrameV1, 2)
	pages := make([]redaction.Page, 2)
	for i := range frames {
		var err error
		frames[i], err = document.NewPDFPageFrame(source, i+1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
		require.NoError(t, err)
		_, frameSHA, err := document.MarshalPageFrameV1(frames[i])
		require.NoError(t, err)
		pages[i] = redaction.Page{Number: i + 1, FrameSHA256: frameSHA, Width: frames[i].Width, Height: frames[i].Height}
	}
	policy, err := document.NewEvidencePolicy(1 << 20)
	require.NoError(t, err)
	unit := func(page, order int, text string) document.SourceEvidenceUnitV1 {
		return document.SourceEvidenceUnitV1{Order: order, Text: text, Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: int64(page), End: int64(page)}, Regions: []document.SourceEvidenceRegionV1{{ProviderID: fmt.Sprintf("ocr-%d", page), Kind: document.EvidenceRegionParagraph, Order: 0, TextRange: document.EvidenceTextRangeV1{Start: 0, End: utf8.RuneCountInString(text)}, Geometry: &document.SourceEvidenceGeometryV1{Boxes: []document.EvidenceBoxV1{{Left: 1, Top: 1, Right: 9, Bottom: 2}}, CoordinateOrigin: document.EvidenceCoordinateTopLeft, CoordinateSpace: document.EvidenceCoordinatePage, Width: 10, Height: 10, Scale: 10, Unit: document.EvidenceGeometryNormalized}}}}
	}
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete, Family: "image", UnitKind: document.EvidenceUnitPage, Units: []document.SourceEvidenceUnitV1{unit(1, 0, "one"), unit(2, 1, "two")}}, policy)
	require.NoError(t, err)
	m, err := alignEvidence(frames, evidence, pages, strings.Repeat("b", 64), evidence.Checksum)
	require.NoError(t, err)
	require.Equal(t, []redaction.Span{{Start: 0, End: 3}, {Start: 3, End: 6}}, []redaction.Span{m.Pages[0].Span, m.Pages[1].Span})
	require.Equal(t, []int{1, 2}, []int{m.Atoms[0].Boxes[0].Page, m.Atoms[1].Boxes[0].Page})
}

func TestAlignEvidenceRejectsPartiallyMappedNonWhitespace(t *testing.T) {
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: strings.Repeat("a", 64), Size: 123}
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
	require.NoError(t, err)
	_, frameSHA, err := document.MarshalPageFrameV1(frame)
	require.NoError(t, err)
	page := redaction.Page{Number: 1, FrameSHA256: frameSHA, Width: frame.Width, Height: frame.Height}
	evidence := scanEvidenceFor(t, "mapped missing")
	evidence.Units[0].Regions[0].TextRange.End = 6

	_, err = alignEvidence([]document.PageFrameV1{frame}, evidence, []redaction.Page{page}, strings.Repeat("b", 64), evidence.Checksum)
	var problem *Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "mapping_incomplete", problem.Code)
}

func TestAlignEvidenceRejectsMissingAndUnsupportedPageLocators(t *testing.T) {
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: strings.Repeat("a", 64), Size: 123}
	frames := make([]document.PageFrameV1, 3)
	pages := make([]redaction.Page, 3)
	for index := range frames {
		var err error
		frames[index], err = document.NewPDFPageFrame(source, index+1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
		require.NoError(t, err)
		_, frameSHA, err := document.MarshalPageFrameV1(frames[index])
		require.NoError(t, err)
		pages[index] = redaction.Page{Number: index + 1, FrameSHA256: frameSHA, Width: frames[index].Width, Height: frames[index].Height}
	}
	evidence := scanEvidenceFor(t, "one")
	for _, mutate := range []func(*document.NormalizedEvidenceV1){
		func(value *document.NormalizedEvidenceV1) {},
		func(value *document.NormalizedEvidenceV1) {
			value.Units[0].Locator.Kind = document.EvidenceLocatorGeneric
		},
		func(value *document.NormalizedEvidenceV1) { value.Completeness = document.EvidencePartial },
	} {
		candidate := evidence
		candidate.Units = append([]document.NormalizedEvidenceUnitV1(nil), evidence.Units...)
		mutate(&candidate)
		_, err := alignEvidence(frames, candidate, pages, strings.Repeat("b", 64), candidate.Checksum)
		var problem *Problem
		require.ErrorAs(t, err, &problem)
		require.Equal(t, "mapping_incomplete", problem.Code)
	}

	middleMissing := evidence
	middleMissing.Units = []document.NormalizedEvidenceUnitV1{evidence.Units[0], evidence.Units[0]}
	middleMissing.Units[1].Locator.Start, middleMissing.Units[1].Locator.End = 3, 3
	_, err := alignEvidence(frames, middleMissing, pages, strings.Repeat("b", 64), middleMissing.Checksum)
	var problem *Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "mapping_incomplete", problem.Code)
}

func TestSemanticUnitsAcceptExactPixelOCRGeometry(t *testing.T) {
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: strings.Repeat("a", 64), Size: 123}
	frame, err := document.NewPNGPageFrame(source, 100, 100, 1000, 1000)
	require.NoError(t, err)
	_, frameSHA, err := document.MarshalPageFrameV1(frame)
	require.NoError(t, err)
	page := redaction.Page{Number: 1, FrameSHA256: frameSHA, Width: frame.Width, Height: frame.Height, Span: redaction.Span{Start: 0, End: 1}}
	evidence := scanEvidenceFor(t, "x")
	geometry := evidence.Units[0].Regions[0].Geometry
	geometry.Unit = document.EvidenceGeometryPixel
	geometry.Scale, geometry.Width, geometry.Height = 1, 100, 100
	geometry.Boxes = []document.EvidenceBoxV1{{Left: 0, Top: 0, Right: 100, Bottom: 100}}
	m, err := alignEvidence([]document.PageFrameV1{frame}, evidence, []redaction.Page{page}, strings.Repeat("b", 64), evidence.Checksum)
	require.NoError(t, err)
	units, err := semanticUnits(m, []document.PageFrameV1{frame}, evidence, evidence.Checksum)
	require.NoError(t, err)
	require.Len(t, units, 1)
	require.Equal(t, []redaction.Span{{Start: 0, End: 1}}, units[0].Spans)
}

func TestOCRSemanticUnitsKeepRepeatedParagraphRegionsDistinct(t *testing.T) {
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: strings.Repeat("a", 64), Size: 123}
	frame, err := document.NewPNGPageFrame(source, 100, 100, 1000, 1000)
	require.NoError(t, err)
	_, frameSHA, err := document.MarshalPageFrameV1(frame)
	require.NoError(t, err)
	page := redaction.Page{Number: 1, FrameSHA256: frameSHA, Width: frame.Width, Height: frame.Height}
	policy, err := document.NewEvidencePolicy(1 << 20)
	require.NoError(t, err)
	geometry := func(top, bottom int64) *document.SourceEvidenceGeometryV1 {
		return &document.SourceEvidenceGeometryV1{Boxes: []document.EvidenceBoxV1{{Left: 0, Top: top, Right: 100, Bottom: bottom}}, CoordinateOrigin: document.EvidenceCoordinateTopLeft, CoordinateSpace: document.EvidenceCoordinatePage, Width: 100, Height: 100, Scale: 1, Unit: document.EvidenceGeometryPixel}
	}
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceComplete,
		Family:          "image",
		UnitKind:        document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{{
			Order: 0,
			Text:  "same same",
			Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
				IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1},
			Regions: []document.SourceEvidenceRegionV1{
				{ProviderID: "paragraph-one", Kind: document.EvidenceRegionParagraph, Order: 0, TextRange: document.EvidenceTextRangeV1{Start: 0, End: 4}, Geometry: geometry(0, 40)},
				{ProviderID: "paragraph-two", Kind: document.EvidenceRegionParagraph, Order: 1, TextRange: document.EvidenceTextRangeV1{Start: 5, End: 9}, Geometry: geometry(60, 100)},
			},
		}},
	}, policy)
	require.NoError(t, err)
	m, err := alignEvidence([]document.PageFrameV1{frame}, evidence, []redaction.Page{page}, strings.Repeat("b", 64), evidence.Checksum)
	require.NoError(t, err)
	units, err := semanticUnits(m, []document.PageFrameV1{frame}, evidence, evidence.Checksum)
	require.NoError(t, err)
	require.Len(t, units, 2)
	require.NotEqual(t, units[0].ID, units[1].ID)
	require.Equal(t, []redaction.Span{{Start: 0, End: 4}}, units[0].Spans)
	require.Equal(t, []redaction.Span{{Start: 5, End: 9}}, units[1].Spans)
}

func TestEvidenceBoxesUseExactCheckedScaling(t *testing.T) {
	frame := document.PageFrameV1{InputUnits: "point/10000", Width: 573681941224033, Height: 100000}
	page := redaction.Page{Number: 1, FrameSHA256: strings.Repeat("a", 64), Width: 100513, Height: 100000}
	geometry := document.EvidenceGeometryV1{Boxes: []document.EvidenceBoxV1{{Left: 429588647783909, Top: 1, Right: 429588647783910, Bottom: 2}}, CoordinateOrigin: document.EvidenceCoordinateTopLeft, CoordinateSpace: document.EvidenceCoordinatePage, Width: 573681941224033, Height: 100000, Scale: 573681941224033, Unit: document.EvidenceGeometryNormalized}
	boxes, err := evidenceBoxes(frame, page, geometry)
	require.NoError(t, err)
	require.Equal(t, int64(75266), boxes[0].X0)
}

func TestRenderTextRenditionDeterministicUnicodeAndSemanticUnits(t *testing.T) {
	text := strings.Repeat("Speaker 1 [00:00:01] café 給与 line with deterministic wrapping.\n", 90)
	units := []redaction.Unit{{ID: "turn:0-1000:0", Kind: "transcript_turn", Spans: []redaction.Span{{Start: 0, End: int64(len(text))}}}}
	firstPDF, firstMap, err := RenderTextRendition(t.Context(), text, units)
	require.NoError(t, err)
	secondPDF, secondMap, err := RenderTextRendition(t.Context(), text, units)
	require.NoError(t, err)
	require.Equal(t, firstPDF, secondPDF)
	require.Equal(t, firstMap, secondMap)
	require.Greater(t, len(firstMap.Pages), 1)
	require.Len(t, firstMap.Units, 1)
	require.Greater(t, len(firstMap.Units[0].Boxes), 1)
	require.NoError(t, redaction.ValidateMap(firstMap))
}

func TestRenderTranscriptKeepsSpeakerTimestampTurnsAcrossPagesAndUnicodeClusters(t *testing.T) {
	first := "Alice [00:00:01] office ﬁle 給与\n"
	second := strings.Repeat("Bob [00:00:02] second turn line\n", 90)
	text := first + second
	units := []redaction.Unit{
		{ID: "transcript_turn:alice-000001", Kind: "transcript_turn", Spans: []redaction.Span{{Start: 0, End: int64(len(first))}}},
		{ID: "transcript_turn:bob-000002", Kind: "transcript_turn", Spans: []redaction.Span{{Start: int64(len(first)), End: int64(len(text))}}},
	}
	_, textMap, err := RenderTextRendition(t.Context(), text, units)
	require.NoError(t, err)
	require.Equal(t, units[0].Spans, textMap.Units[0].Spans)
	require.Greater(t, len(textMap.Units[1].Spans), 1)
	for _, span := range textMap.Units[1].Spans {
		bounded := false
		for _, page := range textMap.Pages {
			bounded = bounded || span.Start >= page.Span.Start && span.End <= page.Span.End
		}
		require.True(t, bounded)
	}
	require.Equal(t, "transcript_turn", textMap.Units[1].Kind)
	pages := map[int]bool{}
	for _, box := range textMap.Units[1].Boxes {
		pages[box.Page] = true
	}
	require.Greater(t, len(pages), 1)
	require.NoError(t, redaction.ValidateMap(textMap))
}

func TestRenderTextSplitsLongParagraphSpansAtPageBoundaries(t *testing.T) {
	text := strings.Repeat("one continuous paragraph without blank lines ", 120)
	_, textMap, err := RenderTextRendition(t.Context(), text, nil)
	require.NoError(t, err)
	require.Greater(t, len(textMap.Pages), 1)
	require.Len(t, textMap.Units, 1)
	require.Len(t, textMap.Units[0].Spans, len(textMap.Pages))
	for index, span := range textMap.Units[0].Spans {
		require.Greater(t, span.End, span.Start)
		require.GreaterOrEqual(t, span.Start, textMap.Pages[index].Span.Start)
		require.LessOrEqual(t, span.End, textMap.Pages[index].Span.End)
	}
}

func TestRenderTextSkipsWhitespaceOnlyParagraphUnits(t *testing.T) {
	text := "Para one.\n\n \n\nPara two."
	pdf, textMap, err := RenderTextRendition(t.Context(), text, nil)
	require.NoError(t, err)
	require.NotEmpty(t, pdf)
	require.Len(t, textMap.Units, 2)
	for _, unit := range textMap.Units {
		require.NotEmpty(t, unit.Boxes)
		require.NotEqual(t, " ", text[unit.Spans[0].Start:unit.Spans[0].End])
	}
	require.NoError(t, redaction.ValidateMap(textMap))
}

func TestRenderTextRenditionRejectsEmptyInvalidAndUnsupportedUnits(t *testing.T) {
	_, _, err := RenderTextRendition(t.Context(), "", nil)
	var problem *Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "unsupported_rendition", problem.Code)
	_, _, err = RenderTextRendition(t.Context(), "abc", []redaction.Unit{{ID: "x", Kind: "email_message", Spans: []redaction.Span{{Start: 0, End: 2}, {Start: 1, End: 3}}}})
	require.Error(t, err)
}

func TestRenderTextRenditionEnforcesAggregateLimitsAndCancellation(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		limits textRenditionLimits
	}{
		{"input bytes", "four", textRenditionLimits{MaxInputBytes: 3, MaxPages: 10, MaxAtoms: 10, MaxMapBytes: 1 << 20, MaxOutputBytes: 1 << 20}},
		{"pages", strings.Repeat("line\n", 80), textRenditionLimits{MaxInputBytes: 1 << 20, MaxPages: 1, MaxAtoms: 1 << 20, MaxMapBytes: 1 << 20, MaxOutputBytes: 1 << 20}},
		{"atoms", "ab", textRenditionLimits{MaxInputBytes: 1 << 20, MaxPages: 10, MaxAtoms: 1, MaxMapBytes: 1 << 20, MaxOutputBytes: 1 << 20}},
		{"map bytes", "mapped", textRenditionLimits{MaxInputBytes: 1 << 20, MaxPages: 10, MaxAtoms: 10, MaxMapBytes: 1, MaxOutputBytes: 1 << 20}},
		{"output bytes", "mapped", textRenditionLimits{MaxInputBytes: 1 << 20, MaxPages: 10, MaxAtoms: 10, MaxMapBytes: 1 << 20, MaxOutputBytes: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := renderTextRendition(t.Context(), test.text, nil, test.limits)
			var problem *Problem
			require.ErrorAs(t, err, &problem)
			require.Equal(t, "rendition_limit_exceeded", problem.Code)
		})
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := RenderTextRendition(canceled, "must not render", nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRenderImageRenditionUsesExactRetainedPixelsAndPhysicalFrame(t *testing.T) {
	var problem *Problem
	img := image.NewNRGBA(image.Rect(0, 0, 30, 40))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, img))
	pixels := encoded.Bytes()
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: sum(pixels), Size: int64(len(pixels))}
	frame, err := document.NewPNGPageFrame(source, 30, 40, 11811, 11811)
	require.NoError(t, err)
	_, frameSHA, err := document.MarshalPageFrameV1(frame)
	require.NoError(t, err)
	recipe := document.PageRecipeV1{Contract: document.PageImageContractV1, DPI: float64(11811*127) / 5000,
		Format: "png", RendererIdentity: document.PageRendererIdentity{Executable: "synthetic", Version: "1", Options: []string{"native"}}}
	_, recipeSHA, err := document.MarshalPageRecipeV1(recipe)
	require.NoError(t, err)
	imageReceipt := document.PageImageV1{Contract: document.PageImageContractV1, Source: source, Page: 1, FrameSHA256: frameSHA, RecipeSHA256: recipeSHA, SHA256: sum(pixels), Size: int64(len(pixels)), Width: 30, Height: 40}
	receiptBytes, _, err := document.MarshalPageImageV1(imageReceipt)
	require.NoError(t, err)
	input := ImageRenditionInput{Frame: frame, Recipe: recipe, Image: imageReceipt, ImageReceipt: receiptBytes, OpenImage: func(context.Context, document.PageImageV1) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(pixels)), nil
	}}
	first, err := RenderImageRendition(t.Context(), input)
	require.NoError(t, err)
	second, err := RenderImageRendition(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.True(t, bytes.Contains(first, []byte("/MediaBox [0 0 7.2 9.5976]")))

	input.ImageReceipt = append(bytes.Clone(receiptBytes), ' ')
	_, err = RenderImageRendition(t.Context(), input)
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "bound_evidence_mismatch", problem.Code)
}

func TestRenderImageRenditionRejectsUnsupportedDensity(t *testing.T) {
	var problem *Problem
	err := ValidateImageRenditionFrame(t.Context(), document.PageFrameV1{})
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "unsupported_rendition", problem.Code)
}

func alignNativeFixture(t *testing.T, pdf []byte, visibleEvidence string) redaction.TextMap {
	t.Helper()
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001", SHA256: sum(pdf), Size: int64(len(pdf))}
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
	require.NoError(t, err)
	evidence := evidenceFor(t, visibleEvidence)
	m, err := Align(t.Context(), AlignmentInput{PDF: pdfproduction.Source{Reader: bytes.NewReader(pdf), Size: int64(len(pdf)), SHA256: sum(pdf)}, Pages: []document.PageFrameV1{frame}, Evidence: evidence, EvidenceSHA256: evidence.Checksum})
	require.NoError(t, err)
	return m
}

func encodePNG(t *testing.T, value image.Image) io.Reader {
	t.Helper()
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, value))
	return bytes.NewReader(encoded.Bytes())
}

func evidenceFor(t *testing.T, text string) document.NormalizedEvidenceV1 {
	t.Helper()
	policy, err := document.NewEvidencePolicy(1 << 20)
	require.NoError(t, err)
	e, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete, Family: "pdf", UnitKind: document.EvidenceUnitPage, Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: text, Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}}}}, policy)
	require.NoError(t, err)
	return e
}

func scanEvidenceFor(t *testing.T, text string) document.NormalizedEvidenceV1 {
	t.Helper()
	policy, err := document.NewEvidencePolicy(1 << 20)
	require.NoError(t, err)
	runes := utf8.RuneCountInString(text)
	e, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete, Family: "image", UnitKind: document.EvidenceUnitPage, Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: text, Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}, Regions: []document.SourceEvidenceRegionV1{{ProviderID: "ocr-line-1", Kind: document.EvidenceRegionParagraph, Order: 0, TextRange: document.EvidenceTextRangeV1{Start: 0, End: runes}, Geometry: &document.SourceEvidenceGeometryV1{Boxes: []document.EvidenceBoxV1{{Left: 100000, Top: 100000, Right: 900000, Bottom: 200000}}, CoordinateOrigin: document.EvidenceCoordinateTopLeft, CoordinateSpace: document.EvidenceCoordinatePage, Width: 1000000, Height: 1000000, Scale: 1000000, Unit: document.EvidenceGeometryNormalized}}}}}}, policy)
	require.NoError(t, err)
	return e
}

func sum(value []byte) string { return fmt.Sprintf("%x", sha256.Sum256(value)) }
