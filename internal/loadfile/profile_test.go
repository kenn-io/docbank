package loadfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func TestReadProfilesAreFixedAndHashStably(t *testing.T) {
	dat, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	assert.Equal(t, '\x14', dat.Field)
	assert.Equal(t, 'þ', dat.Qualifier)
	assert.Equal(t, '®', dat.NewlineInField)
	assert.True(t, dat.HeaderRow)
	assert.Equal(t, "utf-8", dat.Encoding)

	first, err := dat.SHA256()
	require.NoError(t, err)
	second, err := dat.SHA256()
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.True(t, canonical.IsSHA256Hex(first))

	wantColumns := map[string][]string{
		"opt-standard-v1":   {"ImageKey", "VolumeName", "ImagePath", "DocumentBreak", "FolderBreak", "BoxBreak", "PageCount"},
		"opt-pagecount5-v1": {"ImageKey", "VolumeName", "ImagePath", "DocumentBreak", "PageCount", "FolderBreak", "BoxBreak"},
	}
	for _, id := range []string{"csv-rfc4180-v1", "opt-standard-v1", "opt-pagecount5-v1"} {
		profile, readErr := ReadProfile(id)
		require.NoError(t, readErr, id)
		assert.Equal(t, id, profile.ID)
		assert.Equal(t, "utf-8", profile.Encoding)
		if want, ok := wantColumns[id]; ok {
			assert.Equal(t, want, profile.Columns)
		}
	}

	_, err = ReadProfile("dat-concordance-v2")
	require.ErrorIs(t, err, ErrInvalidProfile)
}

func TestReadProfileReturnsIndependentColumns(t *testing.T) {
	first, err := ReadProfile("opt-standard-v1")
	require.NoError(t, err)
	first.Columns[0] = "mutated"
	first.Columns = append(first.Columns, "extra")

	second, err := ReadProfile("opt-standard-v1")
	require.NoError(t, err)
	assert.Equal(t, []string{"ImageKey", "VolumeName", "ImagePath", "DocumentBreak", "FolderBreak", "BoxBreak", "PageCount"}, second.Columns)
}

func TestProfileHashCoversColumns(t *testing.T) {
	profile, err := ReadProfile("opt-standard-v1")
	require.NoError(t, err)
	original, err := profile.SHA256()
	require.NoError(t, err)

	profile.Columns[0] = "DifferentImageKey"
	changed, err := profile.SHA256()
	require.NoError(t, err)
	assert.NotEqual(t, original, changed)
}
