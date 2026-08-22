package document

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewNormalizePolicyReturnsVersionThreeIdentity(t *testing.T) {
	policy, err := NewNormalizePolicy(25_000_000)
	require.NoError(t, err)
	assert.Equal(t, NormalizePolicyIdentity{
		Version:                3,
		MaxDocumentChars:       25_000_000,
		MaxUnitChars:           1_000_000,
		MaxSourceUnitBytes:     4_000_000,
		MaxMetadataSourceBytes: 65_536,
		MaxLinkChars:           2_048,
		MaxChunkRunes:          4_000,
		ChunkOverlap:           200,
		MaxChunks:              20_000,
	}, policy.Identity())
	assert.NoError(t, policy.validate())
}

func TestNormalizePolicyRejectsInvalidConstruction(t *testing.T) {
	_, err := NewNormalizePolicy(0)
	require.Error(t, err)
	_, err = NewNormalizePolicy(-1)
	require.Error(t, err)
	require.Error(t, (NormalizePolicy{}).validate())
}

func TestNormalizePolicyIdentityCannotMutatePolicy(t *testing.T) {
	policy, err := NewNormalizePolicy(25_000_000)
	require.NoError(t, err)

	identity := policy.Identity()
	identity.MaxChunkRunes = 1
	identity.MaxDocumentChars = 1

	assert.Equal(t, 4_000, policy.Identity().MaxChunkRunes)
	assert.Equal(t, 25_000_000, policy.Identity().MaxDocumentChars)
}
