package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
)

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestNativeRedactionWhitespaceBetweenRectangles(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET\nSECRET")
	prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	require.Equal(t, "SECRET\r\nSECRET", prep.Map.Text)
	masks := []RedactionMask{}
	for _, y := range []int64{0, 4000} {
		masks = append(masks, RedactionMask{ID: uuid.New(), Selector: redaction.Selector{
			Kind: "rectangle", MapSHA256: prep.Map.SHA256,
			Boxes: []redaction.Box{{Page: 1, FrameSHA256: prep.Map.Pages[0].FrameSHA256,
				X0: 0, Y0: y, X1: 20000, Y1: y + 4000}},
		}})
	}
	review, err := f.service.ResolveNativePDF(t.Context(), RedactionResolveRequest{
		Preparation: prep, Masks: masks}, runtime)
	require.NoError(t, err)
	var output bytes.Buffer
	err = f.service.RenderNativePDF(t.Context(), RedactionRenderRequest{
		Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256}, runtime,
		func(_ context.Context, reader io.Reader, _ RedactionArchive) error {
			_, err := io.Copy(&output, reader)
			return err
		})
	require.NoError(t, err)
	archive, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	require.NoError(t, err)
	text, err := archive.Open("redacted.txt")
	require.NoError(t, err)
	sidecar, err := io.ReadAll(text)
	require.NoError(t, err)
	require.NoError(t, text.Close())
	require.Equal(t, "[REDACTED]\r\n[REDACTED]\f", string(sidecar))
	pdf, err := archive.Open("redacted.pdf")
	require.NoError(t, err)
	data, err := io.ReadAll(pdf)
	require.NoError(t, err)
	require.NoError(t, pdf.Close())
	require.NoError(t, pdfproduction.VerifySaved(t.Context(), bytes.NewReader(data), int64(len(data)),
		review.Resolved, review.Recipe, nil))
}
