package document

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPageDocumentRejectsMixedSourceGeometryAndIncompleteCount(t *testing.T) {
	png, err := NewPNGPageFrame(pageSource(), 254, 508, 10000, 10000)
	require.NoError(t, err)
	pdf, err := NewPDFPageFrame(pageSource(), 2, [4]float64{0, 0, 72, 72}, [4]float64{0, 0, 72, 72}, 0)
	require.NoError(t, err)
	d := PageDocumentV1{Contract: PageFrameContractV1, Source: pageSource(), PageCount: 2, Frames: []PageFrameV1{png, pdf}}
	require.Error(t, ValidatePageDocumentV1(d))
	d.Frames = d.Frames[:1]
	require.Error(t, ValidatePageDocumentV1(d))
	d.PageCount = 1
	require.NoError(t, ValidatePageDocumentV1(d))
	d.Frames[0].Source.Size++
	require.Error(t, ValidatePageDocumentV1(d))
}
