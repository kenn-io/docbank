package csvpdf

import (
	"encoding/csv"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

func TestConvertPreservesCSVParserCause(t *testing.T) {
	t.Parallel()
	source, _ := sourceFor(t, "\"private-synthetic-value")
	_, err := Convert(t.Context(), source, policyFor(t, DefaultLimits()))
	require.Error(t, err)
	diagnostic, ok := errors.AsType[*ocr.PreparationError](err)
	require.True(t, ok)
	require.EqualError(t, diagnostic, "CSV source has invalid record syntax")
	assert.ErrorIs(t, err, csv.ErrQuote)
}

func TestConvertDescribesGeneratedPDFLimit(t *testing.T) {
	t.Parallel()
	source, _ := sourceFor(t, "synthetic,value\n")
	limits := DefaultLimits()
	limits.MaxPDFBytes = 1
	_, err := Convert(t.Context(), source, policyFor(t, limits))
	diagnostic, ok := errors.AsType[*ocr.PreparationError](err)
	require.True(t, ok)
	assert.EqualError(t, diagnostic, "CSV PDF exceeds byte limit")
}

func TestConvertRetainsSourceCloseCause(t *testing.T) {
	t.Parallel()
	source, reader := sourceFor(t, "synthetic,value\n")
	reader.closeErr = io.ErrClosedPipe
	_, err := Convert(t.Context(), source, policyFor(t, DefaultLimits()))
	require.ErrorIs(t, err, io.ErrClosedPipe)
	diagnostic, ok := errors.AsType[*ocr.PreparationError](err)
	require.True(t, ok)
	assert.EqualError(t, diagnostic, "close CSV source failed")
}
