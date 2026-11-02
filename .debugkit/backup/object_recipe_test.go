package backup

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestDecodeObjectRecipeStopsAtInvalidChunk(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const valid = `{"blob":"` + hash + `","bytes":1}`
	for _, invalid := range []string{
		`null`,
		`{"blob":"` + hash + `","bytes":0}`,
		`{"blob":"invalid","bytes":1}`,
		`{"blob":"` + hash + `","bytes":1,"extra":true}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			raw := []byte(`{"version":1,"blob":"` + hash + `","bytes":258,"chunks":[` +
				valid + `,` + invalid + strings.Repeat(`,`+valid, 256) + `]}`)
			recipe, err := decodeObjectRecipe(raw)
			require.Error(t, err)
			assert.Len(t, recipe.Chunks, 1, "stop before retaining the invalid chunk or its suffix")
		})
	}
}

func TestDecodeObjectRecipeChunkLimit(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const chunk = `{"blob":"` + hash + `","bytes":1}`
	for _, count := range []int{maxObjectChunks, maxObjectChunks + 1} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			var raw bytes.Buffer
			raw.Grow(count*(len(chunk)+1) + 160)
			fmt.Fprintf(&raw, `{"version":1,"blob":"%s","bytes":%d,"chunks":[`, hash, count)
			for i := range count {
				if i > 0 {
					raw.WriteByte(',')
				}
				raw.WriteString(chunk)
			}
			raw.WriteString(`]}`)
			require.Less(t, raw.Len(), maxRecipeBytes, "the byte bound permits this input")
			recipe, err := decodeObjectRecipe(raw.Bytes())
			if count == maxObjectChunks {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			assert.Len(t, recipe.Chunks, maxObjectChunks, "never retain more than the chunk limit")
		})
	}
}

func TestDecodeObjectRecipeTrailingData(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const chunk = `{"blob":"` + hash + `","bytes":1}`
	const recipe = `{"version":1,"blob":"` + hash + `","bytes":2,"chunks":[` + chunk + `,` + chunk + `]}`
	for _, suffix := range []string{"", " \n\t", " [null]", " null", " }"} {
		t.Run(fmt.Sprintf("%q", suffix), func(t *testing.T) {
			_, err := decodeObjectRecipe([]byte(recipe + suffix))
			if strings.TrimSpace(suffix) == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "trailing object recipe data")
			}
		})
	}
}

func TestLoadObjectRecipesRejectsConflictingIdentity(t *testing.T) {
	appender, repo, known := newTestAppenderForSource(t)
	t.Cleanup(appender.Abort)
	first, _, err := appender.Add([]byte("a"))
	require.NoError(t, err)
	second, _, err := appender.Add([]byte("b"))
	require.NoError(t, err)
	whole := pack.ComputeBlobID([]byte("ab")).String()
	var recipeIDs []string
	for _, chunks := range [][2]pack.BlobID{{first, second}, {second, first}} {
		raw := fmt.Sprintf(`{"version":1,"blob":%q,"bytes":2,"chunks":[{"blob":%q,"bytes":1},{"blob":%q,"bytes":1}]}`,
			whole, chunks[0].String(), chunks[1].String())
		id, _, err := appender.Add([]byte(raw))
		require.NoError(t, err)
		recipeIDs = append(recipeIDs, id.String())
	}
	_, _, err = appender.Finish()
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		manifest Manifest
		wantErr  string
	}{
		{
			name: "metadata hash",
			manifest: Manifest{Metadata: &ManifestMetadata{
				Blob: first.String(), Bytes: 2, Recipe: recipeIDs[0],
			}},
			wantErr: "metadata recipe differs from manifest identity or size",
		},
		{
			name: "metadata size",
			manifest: Manifest{Metadata: &ManifestMetadata{
				Blob: whole, Bytes: 3, Recipe: recipeIDs[0],
			}},
			wantErr: "metadata recipe differs from manifest identity or size",
		},
		{
			name:     "distinct recipes for one object",
			manifest: Manifest{Attachments: ManifestAttachments{Recipes: recipeIDs}},
			wantErr:  "conflicting recipes for one logical object",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadObjectRecipes(t.Context(), repo, known, &tc.manifest, testPackExt)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestObjectStreamRejectsChunkLengthMismatch(t *testing.T) {
	appender, repo, known := newTestAppenderForSource(t)
	t.Cleanup(appender.Abort)
	first, _, err := appender.Add([]byte("ab"))
	require.NoError(t, err)
	second, _, err := appender.Add([]byte("c"))
	require.NoError(t, err)
	whole := pack.ComputeBlobID([]byte("abc"))
	// The whole hash and total size still match; only the per-chunk lengths differ.
	raw := fmt.Sprintf(`{"version":1,"blob":%q,"bytes":3,"chunks":[{"blob":%q,"bytes":1},{"blob":%q,"bytes":2}]}`,
		whole.String(), first.String(), second.String())
	recipeID, _, err := appender.Add([]byte(raw))
	require.NoError(t, err)
	_, _, err = appender.Finish()
	require.NoError(t, err)
	recipes, err := loadObjectRecipes(t.Context(), repo, known, &Manifest{
		Attachments: ManifestAttachments{Recipes: []string{recipeID.String()}},
	}, testPackExt)
	require.NoError(t, err)
	stream, err := openObject(t.Context(), repo, known, whole, recipes, testPackExt)
	require.NoError(t, err)
	_, readErr := io.Copy(io.Discard, stream)
	closeErr := stream.Close()
	require.ErrorContains(t, readErr, "chunk length differs from recipe")
	assert.False(t, stream.Verified())
	assert.ErrorIs(t, closeErr, pack.ErrVerificationIncomplete)
}

func TestCaptureObjectRejectsDeclaredSizeMismatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expected int64
		wantErr  string
	}{
		{name: "too small", expected: 1, wantErr: "exceeds declared size"},
		{name: "too large", expected: 3, wantErr: "differs from declared size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			appender, _, _ := newTestAppenderForSource(t)
			t.Cleanup(appender.Abort)
			_, _, _, err := captureObject(t.Context(), strings.NewReader("ab"), tc.expected, nil, appender)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
