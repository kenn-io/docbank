package document_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/internal/scanfixture"
)

func TestCommittedScanCorpusMatchesGenerator(t *testing.T) {
	type manifestEntry struct {
		Name      string `json:"name"`
		File      string `json:"file"`
		MediaType string `json:"media_type"`
		SHA256    string `json:"sha256"`
		Bytes     int64  `json:"bytes"`
	}
	var manifest []manifestEntry
	extensions := map[string]string{
		"application/pdf": ".pdf", "image/jpeg": ".jpg", "image/png": ".png",
		"image/webp": ".webp", "audio/wav": ".wav",
	}
	files := make(map[string][]byte)
	for _, fixture := range scanfixture.Corpus() {
		if fixture.Generated {
			continue
		}
		extension := extensions[fixture.MediaType]
		require.NotEmpty(t, extension, fixture.Name)
		name := fixture.Name + extension
		files[name] = fixture.Bytes
		manifest = append(manifest, manifestEntry{
			Name: fixture.Name, File: name, MediaType: fixture.MediaType,
			SHA256: providerutil.SHA256Hex(fixture.Bytes), Bytes: int64(len(fixture.Bytes)),
		})
	}
	encoded, err := json.Marshal(manifest, json.Deterministic(true), jsontext.WithIndent("  "))
	require.NoError(t, err)
	files["manifest.json"] = append(encoded, '\n')

	// This directory contains only generated fixtures. Build everything before
	// replacing it so a generation error leaves the previous corpus intact.
	const directory = "testdata/scanassessment"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.RemoveAll(directory))
		require.NoError(t, os.MkdirAll(directory, 0o755))
		for name, data := range files {
			require.NoError(t, os.WriteFile(filepath.Join(directory, name), data, 0o644))
		}
	}
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Len(t, entries, len(files), "regenerate the corpus with UPDATE_GOLDEN=1")
	for name, data := range files {
		stored, err := os.ReadFile(filepath.Join(directory, name))
		require.NoError(t, err)
		assert.Equal(t, data, stored, name)
	}
}
