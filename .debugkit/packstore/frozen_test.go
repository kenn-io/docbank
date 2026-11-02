package packstore

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type frozenManifest struct {
	PackID     string       `json:"pack_id"`
	PackFile   string       `json:"pack_file"`
	PackSHA256 string       `json:"pack_sha256"`
	Blobs      []frozenBlob `json:"blobs"`
}

type frozenBlob struct {
	Name          string `json:"name"`
	ContentBase64 string `json:"content_base64"`
	Hash          string `json:"hash"`
	Offset        int64  `json:"offset"`
	StoredLen     int64  `json:"stored_len"`
	RawLen        int64  `json:"raw_len"`
	Flags         uint8  `json:"flags"`
	CRC32C        uint32 `json:"crc32c"`
}

func TestFrozenMsgvaultV1Pack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := filepath.Join("testdata", "msgvault-v1")
	manifestBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	require.NoError(err)
	var manifest frozenManifest
	require.NoError(json.Unmarshal(manifestBytes, &manifest))
	packBytes, err := os.ReadFile(filepath.Join(dir, manifest.PackFile))
	require.NoError(err)
	packSum := sha256.Sum256(packBytes)
	assert.Equal(manifest.PackSHA256, hex.EncodeToString(packSum[:]))

	layout := layoutForStoreTest(t)
	finalPath := layout.PackPath(manifest.PackID)
	require.NoError(os.MkdirAll(filepath.Dir(finalPath), 0o700))
	require.NoError(os.WriteFile(finalPath, packBytes, 0o600))
	resolver := &mapResolver{locations: map[Hash]Location{}}
	for _, blob := range manifest.Blobs {
		hash, parseErr := ParseHash(blob.Hash)
		require.NoError(parseErr)
		entry := IndexEntry{
			Hash: hash, PackID: manifest.PackID, Offset: blob.Offset,
			StoredLen: blob.StoredLen, RawLen: blob.RawLen, Flags: blob.Flags, CRC32C: blob.CRC32C,
		}
		resolver.locations[hash] = Location{Member: true, Pack: &entry}
	}
	store := newStoreForTest(t, resolver, layout)
	for _, blob := range manifest.Blobs {
		hash, parseErr := ParseHash(blob.Hash)
		require.NoError(parseErr)
		want, decodeErr := base64.StdEncoding.DecodeString(blob.ContentBase64)
		require.NoError(decodeErr)
		reader, size, openErr := store.Open(t.Context(), hash)
		require.NoError(openErr, blob.Name)
		got, readErr := io.ReadAll(reader)
		require.NoError(errors.Join(readErr, reader.Close()), blob.Name)
		assert.Equal(int64(len(want)), size, blob.Name)
		assert.Equal(want, got, blob.Name)
	}
}
