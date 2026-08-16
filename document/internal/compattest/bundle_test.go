package compattest

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadVerifiesRawBundleAndMetadata(t *testing.T) {
	bundle, raw, err := Load()
	require.NoError(t, err)
	digest := sha256.Sum256(raw)
	assert.Equal(t, BundleSHA256, hex.EncodeToString(digest[:]))
	assert.Equal(t, 1, bundle.BundleSchema)
	assert.Equal(t, "document-compat-v1", bundle.FixtureID)
	assert.Equal(t, "kenn-io/msgvault#616", bundle.SourcePR)
	assert.Equal(t, "73d6c0b33f74c1fd072a7c0258f1cf1e80054698", bundle.BaselineCommit)
	require.Contains(t, bundle.Sections, "normalization_v2")
	require.Contains(t, bundle.Sections, "mistral_request_fingerprint_v2")
	require.Contains(t, bundle.Sections, "msgvault_profile_policy_v1")
}
