package ocr_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

func TestIdentityFingerprintIsStableAndModelBound(t *testing.T) {
	first, err := ocr.NewIdentity("glmocr", "zai-org/GLM-OCR", "0123456789abcdef")
	require.NoError(t, err)
	second, err := ocr.NewIdentity("glmocr", "zai-org/GLM-OCR", "0123456789abcdef")
	require.NoError(t, err)
	changed, err := ocr.NewIdentity("glmocr", "zai-org/GLM-OCR", "fedcba9876543210")
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Len(t, first.Fingerprint, 64)
	assert.NotEqual(t, first.Fingerprint, changed.Fingerprint)
}

func TestSourceAndErrorContract(t *testing.T) {
	content := io.NopCloser(strings.NewReader("synthetic"))
	source, err := ocr.NewSource(
		content, "application/pdf", 9,
		"64c2bf17f5c5f91d0fdb0e8fcb30eaa671da1557e5d644337bc2a64c0c522f43",
	)
	require.NoError(t, err)
	assert.Equal(t, int64(9), source.Size)

	cause := errors.New("busy")
	providerErr := &ocr.ProviderError{Kind: ocr.ErrorTransient, Cause: cause}
	require.ErrorIs(t, providerErr, cause)
	assert.Equal(t, ocr.ErrorTransient, ocr.ErrorKindOf(providerErr))
	assert.True(t, ocr.IsRetryable(providerErr))
}
