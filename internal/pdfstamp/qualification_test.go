package pdfstamp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPdfcpuQualifiesForCrossDocumentNumbering(t *testing.T) {
	requirePopplerQualification(t)
	one := syntheticPDF(t, 1, "Letter")
	three := syntheticPDF(t, 3, "Letter")
	labels := []string{"TEST000041", "TEST000042", "TEST000043", "TEST000044"}

	stamped := stampWithLabels(t, [][]byte{one, three}, labels)

	visible := readVisibleLabels(t, stamped)
	require.Equal(t, labels, visible)
	assert.Equal(t, "TEST000042", visible[1],
		"the second document begins at 42")
}

func TestPdfcpuQualifiesSupportedPageGeometry(t *testing.T) {
	requirePopplerQualification(t)
	for name, input := range map[string][]byte{
		"a4":         syntheticPDF(t, 1, "A4"),
		"rotated90":  syntheticRotated(t, 90),
		"rotated180": syntheticRotated(t, 180),
		"rotated270": syntheticRotated(t, 270),
		"cropped":    syntheticCropped(t),
		"mixed":      syntheticMixedSize(t),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := stampOne(t, input, "TEST000041", fontFor(name))
			require.NoError(t, err)
			labels := readVisibleLabels(t, out)
			require.Len(t, labels, pageCount(t, input))
			for _, label := range labels {
				assert.Equal(t, "TEST000041", label)
			}
			assertRenderedStampAt(t, out, "bottom-right", 24)
		})
	}
}

func TestPdfcpuRejectsOrSafelyHandlesHostileInputs(t *testing.T) {
	requirePopplerQualification(t)
	for name, input := range map[string][]byte{
		"already_stamped":  syntheticAlreadyStamped(t),
		"unsupported_font": syntheticPDF(t, 1, "Letter"),
		"malformed":        []byte("not a pdf"),
		"encrypted":        syntheticEncrypted(t),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := stampOne(t, input, "TEST000041", fontFor(name))
			if err != nil {
				assert.NotContains(t, err.Error(), "panic")
				return
			}
			assert.Contains(t, readVisibleLabels(t, out), "TEST000041")
			assertRenderedStampAt(t, out, "bottom-right", 24)
		})
	}
}

func TestPdfcpuTinyPageRequiresAdapterPreflight(t *testing.T) {
	requirePopplerQualification(t)
	out, err := stampOne(t, syntheticTinyPage(t), "TEST000041", "Helvetica")
	require.NoError(t, err, "pdfcpu currently reports success for this clipped stamp")
	assert.NotContains(t, readVisibleLabels(t, out), "TEST000041",
		"if pdfcpu begins rejecting or fitting this input, revisit the adapter preflight")
}
