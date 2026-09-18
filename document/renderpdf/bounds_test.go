package renderpdf

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBoundsSourceBytes(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxSourceBytes = 52_428_800
	require.NoError(t, validateLimits(limits))
	limits.MaxSourceBytes = 52_428_801
	require.Error(t, validateLimits(limits))
}

func TestBoundsWorkBytes(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxWorkBytes = 536_870_912
	require.NoError(t, validateLimits(limits))
	limits.MaxWorkBytes = 536_870_913
	require.Error(t, validateLimits(limits))
}

func TestBoundsNormalizedBytes(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxNormalizedBytes = 209_715_200
	require.NoError(t, validateLimits(limits))
	limits.MaxNormalizedBytes = 209_715_201
	require.Error(t, validateLimits(limits))
}

func TestBoundsPDFBytes(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxPDFBytes = 52_428_800
	require.NoError(t, validateLimits(limits))
	limits.MaxPDFBytes = 52_428_801
	require.Error(t, validateLimits(limits))
}

func TestBoundsPages(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxPages = 1_000
	require.NoError(t, validateLimits(limits))
	limits.MaxPages = 1_001
	require.Error(t, validateLimits(limits))
}

func TestBoundsXMLElements(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxXMLElements = 2_000_000
	require.NoError(t, validateLimits(limits))
	limits.MaxXMLElements = 2_000_001
	require.Error(t, validateLimits(limits))
}

func TestBoundsXMLDepth(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxXMLDepth = 256
	require.NoError(t, validateLimits(limits))
	limits.MaxXMLDepth = 257
	require.Error(t, validateLimits(limits))
}

func TestBoundsRuntimeEntries(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxRuntimeEntries = 250_000
	require.NoError(t, validateLimits(limits))
	limits.MaxRuntimeEntries = 250_001
	require.Error(t, validateLimits(limits))
}

func TestBoundsConvertOwnsSourceAndWorkLimits(t *testing.T) {
	original := zipDocument(t, "docx")
	runner := &recordingRunner{identity: testRunnerIdentity,
		output: flatODF(FlatTextKind, "<text:p/>")}
	limits := DefaultLimits()
	limits.MaxSourceBytes = int64(len(original) - 1)
	policy := testPolicyWithLimits(t, runner, limits)
	_, err := Convert(t.Context(), testSource(t, original, docxMediaType), "docx", policy)
	require.ErrorContains(t, err, "source exceeds")

	runner = &recordingRunner{identity: testRunnerIdentity,
		output: flatODF(FlatTextKind, "<text:p/>")}
	limits = DefaultLimits()
	limits.MaxWorkBytes = int64(len(original) - 1)
	policy = testPolicyWithLimits(t, runner, limits)
	_, err = Convert(t.Context(), testSource(t, original, docxMediaType), "docx", policy)
	require.ErrorContains(t, err, "work-byte")
}

func TestBoundsScanOwnsNormalizedBytes(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxNormalizedBytes = 1
	_, err := Scan(flatODF(FlatTextKind, "<text:p/>"), FlatTextKind, limits)
	require.ErrorContains(t, err, "byte limit")
}

func TestBoundsConvertOwnsPageLimit(t *testing.T) {
	normalized := flatODF(FlatTextKind, "<text:p/>")
	runner := &recordingRunner{identity: testRunnerIdentity, output: normalized, pdf: testPDFBytesPages(2)}
	limits := DefaultLimits()
	limits.MaxPages = 1
	policy := testPolicyWithLimits(t, runner, limits)
	_, err := Convert(t.Context(), testSource(t, zipDocument(t, "docx"), docxMediaType), "docx", policy)
	require.ErrorContains(t, err, "page limit")
}
