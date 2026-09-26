package pdfproduction

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/go-pdf/fpdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestTaskBResolvedPaddingIsBurnedExactlyOnce(t *testing.T) {
	for _, dpi := range []int{300, 600} {
		t.Run(strconv.Itoa(dpi), func(t *testing.T) {
			recipe, err := QualifiedRecipeForDPI(dpi)
			require.NoError(t, err)
			textMap := redactiontest.Map("ABCDEFGHIJ")
			decision := redaction.Decision{
				ID: "11111111-1111-4111-8111-111111111111", MemberID: "22222222-2222-4222-8222-222222222222", Action: "redact",
				Selector: redaction.Selector{Kind: "text", MapSHA256: textMap.SHA256, Span: &redaction.Span{Start: 4, End: 5}},
			}
			plan, err := redaction.Resolve(textMap, "redact_selected", []redaction.Decision{decision}, recipe)
			require.NoError(t, err)
			require.Len(t, plan.RedactBoxes, 1)
			got, err := PlanPixels(textMap.Pages[0], plan.RedactBoxes, recipe)
			require.NoError(t, err)
			base, err := PlanPixels(textMap.Pages[0], textMap.Atoms[4].Boxes, recipe)
			require.NoError(t, err)
			want := image.Rect(max(0, base[0].Min.X-2), max(0, base[0].Min.Y-2), min(dpi, base[0].Max.X+2), min(dpi, base[0].Max.Y+2))
			require.Equal(t, []image.Rectangle{want}, got, "resolution adds two pixels and PlanPixels must not add them again")

			pixels := image.NewNRGBA(image.Rect(0, 0, dpi, dpi))
			draw.Draw(pixels, pixels.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
			burned, err := Burn(Raster{Pixels: pixels, Page: textMap.Pages[0], DPI: dpi}, plan.RedactBoxes, recipe)
			require.NoError(t, err)
			require.Equal(t, color.NRGBA{A: 255}, burned.Pixels.NRGBAAt(want.Min.X, want.Min.Y))
			require.Equal(t, color.NRGBA{R: 255, G: 255, B: 255, A: 255}, burned.Pixels.NRGBAAt(want.Max.X, want.Min.Y), "a third renderer-side padding pixel would destroy retained content")
		})
	}
}

func TestTaskBRotatedCroppedHiddenObjectsProduceFreshOutput(t *testing.T) {
	source := taskBRotatedCroppedSource(t)
	writeTaskBEvidence(t, "rotated-cropped-source.pdf", source)
	recipe := qualificationRecipe()
	page := redaction.Page{Number: 1, FrameSHA256: digest([]byte("rotated cropped output frame")), Width: 100000, Height: 75000, Span: redaction.Span{End: 13}}
	engine, err := NewPDFium(recipe)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	raster, err := engine.Render(t.Context(), Source{Reader: bytes.NewReader(source), Size: int64(len(source)), SHA256: digest(source)}, page, recipe)
	require.NoError(t, err)
	require.NoError(t, engine.Close())

	mask := redaction.Box{Page: 1, FrameSHA256: page.FrameSHA256, X1: 50000, Y1: page.Height}
	burned, err := Burn(raster, []redaction.Box{mask}, recipe)
	require.NoError(t, err)
	maskPixels, err := PlanPixels(page, []redaction.Box{mask}, recipe)
	require.NoError(t, err)
	boundary := maskPixels[0].Max.X
	require.Equal(t, color.NRGBA{A: 255}, burned.Pixels.NRGBAAt(boundary-1, 100))
	require.Equal(t, raster.Pixels.NRGBAAt(boundary, 100), burned.Pixels.NRGBAAt(boundary, 100), "the first retained pixel adjacent to the mask must survive")

	runBox := redaction.Box{Page: 1, FrameSHA256: page.FrameSHA256, X0: 60000, Y0: 10000, X1: 95000, Y1: 16000}
	plan, artifact := taskBFreshArtifact(t, page, burned.Pixels, []redaction.Box{mask}, []redaction.Run{{Kind: "text", Text: "𠮷 retained", Page: 1, SourceSpan: &redaction.Span{End: 13}, Boxes: []redaction.Box{runBox}, FontSizeMilliPoints: 11000}}, recipe)
	var output bytes.Buffer
	require.NoError(t, WriteFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{artifact}}, plan, recipe))
	require.NoError(t, VerifyFreshWithRecipe(t.Context(), bytes.NewReader(output.Bytes()), int64(output.Len()), verificationFor([]PageArtifact{artifact}), recipe))
	require.Equal(t, "𠮷 retained", extractIndependent(t, output.Bytes()))
	for _, canary := range []string{"TASK-B-HIDDEN-CANARY", "/ActualText", "/EmbeddedFile", "/Metadata", "/Rotate", "/CropBox"} {
		require.NotContains(t, output.String(), canary)
	}
	writeTaskBEvidence(t, "rotated-cropped-sanitized.pdf", output.Bytes())
}

func TestTaskBImageOnlyPartialOverlappingMasks(t *testing.T) {
	recipe, _, raster := taskBImageOnlyRaster(t)
	page := raster.Page
	masks := []redaction.Box{
		{Page: 1, FrameSHA256: page.FrameSHA256, X1: 7000, Y1: 7000},
		{Page: 1, FrameSHA256: page.FrameSHA256, X0: 3000, Y0: 3000, X1: 10000, Y1: 10000},
	}
	burned, err := Burn(raster, masks, recipe)
	require.NoError(t, err)
	require.Equal(t, color.NRGBA{A: 255}, burned.Pixels.NRGBAAt(30, 30), "first-mask-only pixels must be black")
	require.Equal(t, color.NRGBA{A: 255}, burned.Pixels.NRGBAAt(150, 150), "overlap pixels must be black")
	require.Equal(t, color.NRGBA{A: 255}, burned.Pixels.NRGBAAt(270, 270), "second-mask-only pixels must be black")
	require.Equal(t, raster.Pixels.NRGBAAt(270, 30), burned.Pixels.NRGBAAt(270, 30), "pixels outside both masks must be retained")
	require.NotEqual(t, color.NRGBA{A: 255}, burned.Pixels.NRGBAAt(270, 30), "retained pixels must remain non-black")
}

func TestTaskBImageOnlyFullPageMaskFreshOutput(t *testing.T) {
	recipe, page, raster := taskBImageOnlyRaster(t)
	mask := redaction.Box{Page: 1, FrameSHA256: page.FrameSHA256, X1: 10000, Y1: 10000}
	burned, err := Burn(raster, []redaction.Box{mask}, recipe)
	require.NoError(t, err)
	for y := range 300 {
		for x := range 300 {
			require.Equal(t, color.NRGBA{A: 255}, burned.Pixels.NRGBAAt(x, y))
		}
	}
	marker := redaction.Run{Kind: "redaction", Text: "[REDACTED]", Page: 1, Boxes: []redaction.Box{mask}, FontSizeMilliPoints: 11000}
	plan, artifact := taskBFreshArtifact(t, page, burned.Pixels, []redaction.Box{mask}, []redaction.Run{marker}, recipe)
	var output bytes.Buffer
	require.NoError(t, WriteFresh(t.Context(), &output, &artifactSequence{pages: []PageArtifact{artifact}}, plan, recipe))
	require.NoError(t, VerifyFreshWithRecipe(t.Context(), bytes.NewReader(output.Bytes()), int64(output.Len()), verificationFor([]PageArtifact{artifact}), recipe))
	require.Equal(t, "[REDACTED]", extractIndependent(t, output.Bytes()))
	writeTaskBEvidence(t, "image-only-full-mask.pdf", output.Bytes())
}

func taskBImageOnlyRaster(t *testing.T) (redaction.Recipe, redaction.Page, Raster) {
	t.Helper()
	recipe := qualificationRecipe()
	page := redaction.Page{Number: 1, FrameSHA256: digest([]byte("image-only output frame")), Width: 10000, Height: 10000}
	source := taskBImageOnlySource(t)
	engine, err := NewPDFium(recipe)
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	raster, err := engine.Render(t.Context(), Source{Reader: bytes.NewReader(source), Size: int64(len(source)), SHA256: digest(source)}, page, recipe)
	require.NoError(t, err)
	return recipe, page, raster
}

func TestTaskBQualifiedLimitsAndSinglePageBuffering(t *testing.T) {
	for _, dpi := range []int{300, 600} {
		recipe, err := QualifiedRecipeForDPI(dpi)
		require.NoError(t, err)
		require.Equal(t, int64(512<<20), recipe.WASMMemoryBytes)
		require.Equal(t, int64(60), recipe.PageTimeoutSeconds)
		width, height := int64(8000)*10000/int64(dpi), int64(5000)*10000/int64(dpi)
		_, _, err = dimensions(redaction.Page{Number: 1, FrameSHA256: digest([]byte("forty million pixels")), Width: width, Height: height}, recipe)
		require.NoError(t, err)
		_, _, err = dimensions(redaction.Page{Number: 1, FrameSHA256: digest([]byte("over forty million pixels")), Width: width + 10000/int64(dpi), Height: height}, recipe)
		require.Error(t, err)
		axis := int64(16384) * 10000 / int64(dpi)
		_, _, err = dimensions(redaction.Page{Number: 1, FrameSHA256: digest([]byte("axis boundary")), Width: axis, Height: 1}, recipe)
		require.NoError(t, err)
		_, _, err = dimensions(redaction.Page{Number: 1, FrameSHA256: digest([]byte("axis overflow")), Width: axis + 1, Height: 1}, recipe)
		require.Error(t, err)
	}

	recipe := qualificationRecipe()
	recipe.PageTimeoutSeconds = 1
	sequence := &blockingTaskBSequence{}
	var output bytes.Buffer
	err := writeFresh(t.Context(), &output, sequence, recipe)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	first := unicodeArtifact(t)
	second := renumberArtifact(t, first, 2)
	tracked := &taskBStrictPageSequence{pages: []PageArtifact{first, second}}
	output.Reset()
	require.NoError(t, writeFresh(t.Context(), &output, tracked, qualificationRecipe()))
	require.False(t, tracked.open, "the final page reader must close before EOF is requested")
}

func taskBFreshArtifact(t *testing.T, page redaction.Page, pixels image.Image, masks []redaction.Box, runs []redaction.Run, recipe redaction.Recipe) (redaction.Resolved, PageArtifact) {
	t.Helper()
	recipeBytes, err := canonical.Marshal(recipe)
	require.NoError(t, err)
	plan := redaction.Resolved{Contract: "redaction-plan/v1", MapSHA256: digest([]byte("task B synthetic map")), RecipeSHA256: digest(recipeBytes), PageCount: 1, Pages: []redaction.Page{page}, Gaps: []redaction.Gap{}, RedactBoxes: masks, Removed: []redaction.Span{}, Runs: runs, UncertainDecisionIDs: []string{}, Regions: []redaction.RedactionRegion{}}
	sealPlan(t, &plan)
	layout := redaction.PageLayout{Source: page, Output: page}
	artifact := PageArtifact{Page: page, ResolvedSHA256: plan.SHA256, Runs: runs, Layout: layout, LayoutSHA256: mustDigest(t, layout), Endorsements: []redaction.Endorsement{}, EndorsementsSHA256: digest([]byte("[]"))}
	encodeArtifact(t, &artifact, pixels)
	return plan, artifact
}

func taskBRotatedCroppedSource(t *testing.T) []byte {
	t.Helper()
	const canary = "TASK-B-HIDDEN-CANARY"
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 612, Ht: 792}})
	pdf.SetCatalogSort(true)
	pdf.SetCompression(false)
	pdf.SetSubject(canary, false)
	pdf.SetAttachments([]fpdf.Attachment{{Content: []byte(canary), Filename: "hidden.bin"}})
	pdf.AddPage()
	pdf.SetPageBox("crop", 36, 36, 540, 720)
	pdf.SetFont("Helvetica", "", 14)
	pdf.RawWriteStr("/Span << /ActualText (" + canary + ") >> BDC")
	pdf.Text(72, 72, "VISIBLE SYNTHETIC")
	pdf.RawWriteStr("EMC")
	var unrotated bytes.Buffer
	require.NoError(t, pdf.Output(&unrotated))
	configuration := model.NewDefaultConfiguration()
	configuration.Offline = true
	var rotated bytes.Buffer
	require.NoError(t, api.Rotate(bytes.NewReader(unrotated.Bytes()), &rotated, 90, nil, configuration))
	require.Contains(t, rotated.String(), canary)
	return rotated.Bytes()
}

func taskBImageOnlySource(t *testing.T) []byte {
	t.Helper()
	imagePage := image.NewNRGBA(image.Rect(0, 0, 300, 300))
	draw.Draw(imagePage, imagePage.Bounds(), image.NewUniform(color.NRGBA{R: 210, G: 70, B: 40, A: 255}), image.Point{}, draw.Src)
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, imagePage))
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 72, Ht: 72}})
	pdf.AddPage()
	options := fpdf.ImageOptions{ImageType: "PNG", ReadDpi: false}
	pdf.RegisterImageOptionsReader("synthetic-image-only", options, bytes.NewReader(encoded.Bytes()))
	pdf.ImageOptions("synthetic-image-only", 0, 0, 72, 72, false, options, 0, "")
	var output bytes.Buffer
	require.NoError(t, pdf.Output(&output))
	return output.Bytes()
}

type blockingTaskBSequence struct{}

func (*blockingTaskBSequence) Next(ctx context.Context) (PageArtifact, error) {
	<-ctx.Done()
	return PageArtifact{}, ctx.Err()
}

type taskBStrictPageSequence struct {
	pages []PageArtifact
	index int
	open  bool
}

func (s *taskBStrictPageSequence) Next(context.Context) (PageArtifact, error) {
	if s.open {
		return PageArtifact{}, errors.New("next page requested before prior PNG reader closed")
	}
	if s.index == len(s.pages) {
		return PageArtifact{}, io.EOF
	}
	artifact := s.pages[s.index]
	s.index++
	open := artifact.OpenPNG
	artifact.OpenPNG = func() (io.ReadCloser, error) {
		if s.open {
			return nil, errors.New("two page buffers opened concurrently")
		}
		reader, err := open()
		if err != nil {
			return nil, err
		}
		s.open = true
		return &taskBTrackingReader{ReadCloser: reader, close: func() { s.open = false }}, nil
	}
	return artifact, nil
}

type taskBTrackingReader struct {
	io.ReadCloser

	close func()
}

func (r *taskBTrackingReader) Close() error {
	err := r.ReadCloser.Close()
	r.close()
	return err
}

func writeTaskBEvidence(t *testing.T, name string, data []byte) {
	t.Helper()
	directory := os.Getenv("DOCBANK_TASK_B_EVIDENCE_DIR")
	if directory == "" {
		return
	}
	require.NoError(t, os.MkdirAll(directory, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, name), data, 0o600))
}
