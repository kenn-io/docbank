package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
)

func redactionSourcePDF(t *testing.T, fixture redactionFixture) []byte {
	t.Helper()
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.AddPage()
	pdf.SetFont(fixture.font, "", 12)
	pdf.Text(fixture.x, fixture.y, fixture.text)
	var output bytes.Buffer
	require.NoError(t, pdf.Output(&output))
	return output.Bytes()
}

func redactionPDFConfig() *model.Configuration {
	return &model.Configuration{Reader15: true, ValidationMode: model.ValidationRelaxed,
		Offline: true, Limits: model.DefaultResourceLimits()}
}

func readRedactionPDFText(t *testing.T, data []byte) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	runtimeConfig := wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling).
		WithMemoryLimitPages(8192).WithCloseOnContextDone(true)
	pool, err := webassembly.Init(webassembly.Config{
		Context: ctx, FSConfig: wazero.NewFSConfig(), RuntimeConfig: runtimeConfig, MaxTotal: 1,
	})
	require.NoError(t, err)
	// Return retired reader memory before the next PDF operation checks process RSS.
	defer debug.FreeOSMemory()
	defer func() { require.NoError(t, pool.Close()) }()
	instance, err := pool.GetInstanceWithContext(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, instance.Close()) }()
	doc, err := instance.OpenDocument(&requests.OpenDocument{File: &data})
	require.NoError(t, err)
	defer func() {
		_, closeErr := instance.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
		require.NoError(t, closeErr)
	}()
	result, err := instance.GetPageText(&requests.GetPageText{
		Page: requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: 0}},
	})
	require.NoError(t, err)
	return strings.TrimSpace(result.Text)
}

func writeRedactionPDF(t *testing.T, path string, source pdfproduction.Source,
	plan redaction.Resolved,
) (pdfproduction.PageArtifact, image.Image) {
	t.Helper()
	recipe := pdfproduction.QualifiedRecipe()
	engine, err := pdfproduction.NewPDFium(recipe)
	require.NoError(t, err)
	raster, renderErr := engine.Render(t.Context(), source, plan.Pages[0], recipe)
	closeErr := engine.Close()
	require.NoError(t, renderErr)
	require.NoError(t, closeErr)
	require.Equal(t, image.Rect(0, 0, 2550, 3300), raster.Pixels.Bounds())
	burned, err := pdfproduction.Burn(raster, plan.RedactBoxes, recipe)
	require.NoError(t, err)
	var pngBytes bytes.Buffer
	require.NoError(t, png.Encode(&pngBytes, burned.Pixels))
	page := plan.Pages[0]
	layout := redaction.PageLayout{Source: page, Output: page}
	layoutBytes, err := canonical.Marshal(layout)
	require.NoError(t, err)
	endorsements := []redaction.Endorsement{}
	endorsementBytes, err := canonical.Marshal(endorsements)
	require.NoError(t, err)
	artifact := pdfproduction.PageArtifact{
		Page: page, Layout: layout, LayoutSHA256: redactionPDFHash(layoutBytes),
		Endorsements: endorsements, EndorsementsSHA256: redactionPDFHash(endorsementBytes),
		ResolvedSHA256: plan.SHA256, Runs: plan.Runs,
		PNGSHA256: redactionPDFHash(pngBytes.Bytes()), PNGSize: int64(pngBytes.Len()),
		OpenPNG: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(pngBytes.Bytes())), nil
		},
	}
	output, err := os.Create(path)
	require.NoError(t, err)
	writeErr := pdfproduction.WriteFresh(t.Context(), output,
		&redactionPageSequence{artifact: artifact}, plan, recipe)
	closeErr = output.Close()
	require.NoError(t, writeErr)
	require.NoError(t, closeErr)
	return artifact, raster.Pixels
}

func verifyRedactionPDF(t *testing.T, pdf, sidecar []byte, artifact pdfproduction.PageArtifact) {
	t.Helper()
	sequence := &redactionPageSequence{artifact: artifact}
	textRead, endorsementsRead := false, false
	err := pdfproduction.VerifyFresh(t.Context(), bytes.NewReader(pdf), int64(len(pdf)),
		pdfproduction.VerificationInput{
			Pages: []redaction.Page{artifact.Page}, ResolvedSHA256: []string{artifact.ResolvedSHA256},
			LayoutSHA256:       []string{artifact.LayoutSHA256},
			EndorsementsSHA256: []string{artifact.EndorsementsSHA256},
			NextPageArtifact:   sequence.Next,
			NextText: func(context.Context) (int, []byte, error) {
				if textRead {
					return 0, nil, io.EOF
				}
				textRead = true
				return 1, sidecar[:len(sidecar)-1], nil
			},
			NextEndorsements: func(context.Context) (
				redaction.PageLayout, []redaction.Endorsement, error,
			) {
				if endorsementsRead {
					return redaction.PageLayout{}, nil, io.EOF
				}
				endorsementsRead = true
				return artifact.Layout, artifact.Endorsements, nil
			},
		})
	require.NoError(t, err)
}

type redactionPageSequence struct {
	artifact pdfproduction.PageArtifact
	read     bool
}

func (s *redactionPageSequence) Next(context.Context) (pdfproduction.PageArtifact, error) {
	if s.read {
		return pdfproduction.PageArtifact{}, io.EOF
	}
	s.read = true
	return s.artifact, nil
}

func assertRedactionPixels(t *testing.T, pdf []byte, original image.Image, fixture redactionFixture) {
	t.Helper()
	pages, err := pdfapi.ExtractImagesRaw(bytes.NewReader(pdf), nil, redactionPDFConfig())
	require.NoError(t, err)
	require.Len(t, pages, 1)
	for _, images := range pages {
		require.Len(t, images, 1)
		for _, raw := range images {
			decoded, _, err := image.Decode(raw.Reader)
			require.NoError(t, err)
			require.Equal(t, image.Rect(0, 0, 2550, 3300), decoded.Bounds())
			white := color.RGBA64{R: 65535, G: 65535, B: 65535, A: 65535}
			require.Equal(t, white, color.RGBA64Model.Convert(original.At(fixture.black.Min.X, fixture.black.Min.Y)))
			// Literal regions follow the fixture's placement, not the resolved mask.
			for y := fixture.black.Min.Y; y < fixture.black.Max.Y; y++ {
				for x := fixture.black.Min.X; x < fixture.black.Max.X; x++ {
					require.Equal(t, color.RGBA64{A: 65535},
						color.RGBA64Model.Convert(decoded.At(x, y)), "mask pixel %d,%d", x, y)
				}
			}
			for _, region := range fixture.retained {
				ink := 0
				for y := region.Min.Y; y < region.Max.Y; y++ {
					for x := region.Min.X; x < region.Max.X; x++ {
						before := color.RGBA64Model.Convert(original.At(x, y))
						require.Equal(t, before, color.RGBA64Model.Convert(decoded.At(x, y)),
							"retained pixel %d,%d", x, y)
						if before != white {
							ink++
						}
					}
				}
				require.Positive(t, ink, "retained region %v must contain ink", region)
			}
		}
	}
}
