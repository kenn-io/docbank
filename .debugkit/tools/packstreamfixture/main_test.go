package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestWriteFixtureContainsRawAndCompressedEntries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "stream-v1.pack")
	require.NoError(writeFixture(path))
	reader, err := pack.OpenReader(path, nil)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reader.Close()) })
	entries := reader.Entries()
	require.Len(entries, 2)
	assert.Zero(entries[0].Flags & pack.BlobCompressed)
	assert.NotZero(entries[1].Flags & pack.BlobCompressed)
}
