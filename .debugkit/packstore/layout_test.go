package packstore_test

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func testHash() string {
	sum := sha256.Sum256([]byte("alpha"))
	return hex.EncodeToString(sum[:])
}

func TestParseHashRejectsNonCanonicalValues(t *testing.T) {
	valid := testHash()
	for _, tt := range []struct {
		name  string
		value string
		ok    bool
	}{
		{name: "canonical", value: valid, ok: true},
		{name: "empty", value: ""},
		{name: "short", value: valid[:63]},
		{name: "uppercase", value: "A" + valid[1:]},
		{name: "separator", value: valid[:31] + "/" + valid[32:]},
		{name: "non-hex", value: "g" + valid[1:]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			hash, err := packstore.ParseHash(tt.value)
			if tt.ok {
				require.NoError(err)
				assert.Equal(tt.value, hash.String())
				return
			}
			require.ErrorIs(err, packstore.ErrInvalidHash)
			assert.Empty(hash)
		})
	}
}

func TestLayoutBuildsCanonicalPaths(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	root := t.TempDir()
	hash, err := packstore.ParseHash(testHash())
	require.NoError(err)
	packID := pack.NewPackID()

	sameDir, err := packstore.NewLayout(root, packstore.LayoutOptions{
		Staging: packstore.StagingSameDirectory,
	})
	require.NoError(err)
	assert.Equal(filepath.Join(root, hash.String()[:2], hash.String()), sameDir.LoosePath(hash))
	assert.Equal(filepath.Join(root, hash.String()[:2]), sameDir.LooseStagingDir(hash))
	assert.Equal(filepath.Join(root, "packs", packID[:2], packID+packstore.PackExt), sameDir.PackPath(packID))

	storeTemp, err := packstore.NewLayout(root, packstore.LayoutOptions{
		Staging:    packstore.StagingStoreDirectory,
		StagingDir: "tmp",
	})
	require.NoError(err)
	assert.Equal(filepath.Join(root, "tmp"), storeTemp.LooseStagingDir(hash))

	rootTemp, err := packstore.NewLayout(root, packstore.LayoutOptions{
		Staging:    packstore.StagingStoreDirectory,
		StagingDir: ".",
	})
	require.NoError(err)
	assert.Equal(root, rootTemp.LooseStagingDir(hash))
}

func TestLayoutCompressedLoosePath(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	hash, err := packstore.ParseHash(testHash())
	require.NoError(err)
	layout, err := packstore.NewLayout(root, packstore.LayoutOptions{
		Staging: packstore.StagingSameDirectory,
	})
	require.NoError(err)

	assert.Equal(t, layout.LoosePath(hash)+".zst", layout.CompressedLoosePath(hash))
	assert.Empty(t, layout.CompressedLoosePath(packstore.Hash("invalid")))
}

func TestLayoutRejectsUnsafeConfigurationAndPackIDs(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	for _, stagingDir := range []string{"", "../tmp", "a/b", "/tmp"} {
		_, err := packstore.NewLayout(root, packstore.LayoutOptions{
			Staging:    packstore.StagingStoreDirectory,
			StagingDir: stagingDir,
		})
		require.Error(err, stagingDir)
	}
	_, err := packstore.NewLayout("", packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.Error(err)

	layout, err := packstore.NewLayout(root, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(err)
	assert.Empty(t, layout.PackPath("../../outside"))
}

func TestDefaultLimitsMatchMsgvaultCompatibilityCeilings(t *testing.T) {
	assert.Equal(t, packstore.Limits{
		BlobBytes:   64 << 20,
		PackBytes:   128 << 20,
		FooterBytes: 8 << 20,
		PackEntries: 100_000,
	}, packstore.DefaultLimits())
}
