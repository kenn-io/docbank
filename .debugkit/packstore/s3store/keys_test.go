package s3store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestCanonicalKeysStayInsideConfiguredPrefix(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	hash := packstore.Hash("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	keys, err := newKeyspace("archives/demo")
	require.NoError(t, err)

	assert.Equal("archives/demo/", keys.join(""))
	assert.Equal("archives/demo/.kit-store.json", keys.ownership())
	assert.Equal("archives/demo/loose/01/"+hash.String(), keys.loose(hash, packstore.LooseEncodingRaw))
	assert.Equal("archives/demo/loose/01/"+hash.String()+".zst", keys.loose(hash, packstore.LooseEncodingZstd))
	assert.Equal("archives/demo/packs/01/0123456789abcdef0123456789abcdef.pack",
		keys.pack("0123456789abcdef0123456789abcdef"))
	assert.Equal("archives/demo/staging/epoch/op/part", keys.staging("epoch", "op", "part"))
}

func TestKeyspaceRejectsNonCanonicalPrefixes(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{"/absolute", "../escape", "a//b", "a/./b", "a/../b", `a\b`} {
		_, err := newKeyspace(prefix)
		assert.Error(t, err, prefix)
	}
}

func TestObjectRefRejectsMalformedShortPackIDs(t *testing.T) {
	t.Parallel()
	keys, err := newKeyspace("archives/demo")
	require.NoError(t, err)

	for _, key := range []string{
		"archives/demo/packs/x/.pack",
		"archives/demo/packs/x/a.pack",
	} {
		ref, ok := keys.objectRef(key)
		assert.False(t, ok, key)
		assert.Equal(t, packstore.ObjectRef{}, ref, key)
	}
}
