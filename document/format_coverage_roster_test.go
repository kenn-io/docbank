package document

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func TestPendingRosterMatchesItsFixture(t *testing.T) {
	roster := PendingFormats()
	require.Len(t, roster, 35)
	require.True(t, slices.IsSortedFunc(roster, func(left, right PendingFormatV1) int {
		return compareStrings(left.Label, right.Label)
	}))
	ownerPattern := regexp.MustCompile(`^DB-[0-9]+[a-z]$`)
	catalogExtensions := map[string]string{}
	for _, format := range FormatMetadataCatalog() {
		for _, extension := range format.Extensions {
			catalogExtensions[extension] = format.ID
		}
	}
	for _, pending := range roster {
		assert.NotEmpty(t, pending.Label)
		assert.NotEmpty(t, pending.Note)
		assert.True(t, ownerPattern.MatchString(pending.OwnerSlice), "%s owner %q", pending.Label, pending.OwnerSlice)
		for _, extension := range pending.Extensions {
			assert.NotContains(t, catalogExtensions, extension,
				"%s extension %q already resolves to catalog row %q", pending.Label, extension, catalogExtensions[extension])
		}
	}
	lefIndex := slices.IndexFunc(roster, func(pending PendingFormatV1) bool { return pending.Label == "LEF" })
	require.NotEqual(t, -1, lefIndex)
	assert.Equal(t, "LEF container catalog support is owned by DB-40c.", roster[lefIndex].Note)

	encoded, err := canonical.Marshal(roster)
	require.NoError(t, err)
	expected, err := os.ReadFile(filepath.Join("testdata", "format_roster.json"))
	require.NoError(t, err)
	expected = bytes.TrimSuffix(expected, []byte("\n"))
	assert.Equal(t, string(expected), string(encoded))
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
