package document

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPendingFormatsAreSortedAndSeparateFromCatalog(t *testing.T) {
	roster := PendingFormats()
	require.NotEmpty(t, roster)
	require.True(t, slices.IsSortedFunc(roster, func(left, right PendingFormatV1) int {
		return compareStrings(left.Label, right.Label)
	}))
	catalogExtensions := map[string]string{}
	for _, format := range FormatMetadataCatalog() {
		for _, extension := range format.Extensions {
			catalogExtensions[extension] = format.ID
		}
	}
	for _, pending := range roster {
		assert.NotEmpty(t, pending.Label)
		assert.NotEmpty(t, pending.Note)
		for _, extension := range pending.Extensions {
			assert.NotContains(t, catalogExtensions, extension,
				"%s extension %q already resolves to catalog row %q", pending.Label, extension, catalogExtensions[extension])
		}
	}
}

func TestPendingFormatsReturnsADeepCopy(t *testing.T) {
	first := PendingFormats()
	require.NotEmpty(t, first)
	require.NotEmpty(t, first[0].Extensions)
	first[0].Label = "FORGED"
	first[0].Extensions[0] = "forged"

	second := PendingFormats()
	assert.NotEqual(t, "FORGED", second[0].Label)
	assert.NotEqual(t, "forged", second[0].Extensions[0])
}

func compareStrings(left, right string) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}
