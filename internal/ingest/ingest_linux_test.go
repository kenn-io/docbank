//go:build linux

package ingest

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddTreeRejectsNonUTF8FilenameWithoutPoisoningMetadata(t *testing.T) {
	ing := newTestIngester(t)
	source := t.TempDir()
	badName := "invalid-" + string([]byte{0xff}) + ".txt"
	require.False(t, utf8.ValidString(badName))
	badPath := filepath.Join(source, badName)
	require.NoError(t, os.WriteFile(badPath, []byte("must not import"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "valid.txt"), []byte("import me"), 0o600))

	preflight, err := Preflight(t.Context(), []string{source}, Options{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), preflight.Files)
	assert.Equal(t, int64(1), preflight.Errors)
	require.Len(t, preflight.Findings, 1)
	assert.Equal(t, strconv.QuoteToASCII(badPath), preflight.Findings[0].Path)

	rep, err := ing.AddPaths(t.Context(), []string{source}, "/archive")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Added)
	require.Len(t, rep.Failed, 1)
	assert.Equal(t, strconv.QuoteToASCII(badPath), rep.Failed[0].Path)
	require.ErrorContains(t, rep.Failed[0].Err, "is not valid UTF-8")
	_, err = ing.Store.NodeByPath(t.Context(),
		filepath.Join("/archive", filepath.Base(source), "valid.txt"))
	require.NoError(t, err)

	var metadata bytes.Buffer
	require.NoError(t, ing.Store.ExportMetadata(t.Context(), &metadata))
	assert.True(t, utf8.Valid(metadata.Bytes()))
}
