package embedding_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/embedding"
)

func TestEgressFingerprintSeparatesPurposeAndDestination(t *testing.T) {
	base := embedding.EgressIdentity{
		Purpose: embedding.EgressDocumentEmbedding, Provider: "synthetic",
		Endpoint: "HTTPS://EXAMPLE.COM/v1/", Model: "embed-1", ModelRevision: "2026-08",
	}
	first, err := base.Fingerprint()
	require.NoError(t, err)
	canonical, err := (embedding.EgressIdentity{
		Purpose: embedding.EgressDocumentEmbedding, Provider: "synthetic",
		Endpoint: "https://example.com/v1/", Model: "embed-1", ModelRevision: "2026-08",
	}).Fingerprint()
	require.NoError(t, err)
	assert.Equal(t, first, canonical)

	query := base
	query.Purpose = embedding.EgressQueryEmbedding
	queryFingerprint, err := query.Fingerprint()
	require.NoError(t, err)
	assert.NotEqual(t, first, queryFingerprint)

	redirected := base
	redirected.Endpoint = "https://other.example/v1"
	redirectedFingerprint, err := redirected.Fingerprint()
	require.NoError(t, err)
	assert.NotEqual(t, first, redirectedFingerprint)

	escaped := base
	escaped.Endpoint = "https://example.com/tenant%2Fprivate"
	escapedFingerprint, err := escaped.Fingerprint()
	require.NoError(t, err)
	literal := base
	literal.Endpoint = "https://example.com/tenant/private"
	literalFingerprint, err := literal.Fingerprint()
	require.NoError(t, err)
	assert.NotEqual(t, escapedFingerprint, literalFingerprint)

	encodedDots := base
	encodedDots.Endpoint = "https://example.com/a/%2e%2e/private"
	encodedDotsFingerprint, err := encodedDots.Fingerprint()
	require.NoError(t, err)
	cleaned := base
	cleaned.Endpoint = "https://example.com/private"
	cleanedFingerprint, err := cleaned.Fingerprint()
	require.NoError(t, err)
	assert.NotEqual(t, encodedDotsFingerprint, cleanedFingerprint)

	root := base
	root.Endpoint = "https://example.com/"
	rootFingerprint, err := root.Fingerprint()
	require.NoError(t, err)
	emptyPath := base
	emptyPath.Endpoint = "https://example.com"
	emptyPathFingerprint, err := emptyPath.Fingerprint()
	require.NoError(t, err)
	assert.Equal(t, rootFingerprint, emptyPathFingerprint)
	escapedRoot := base
	escapedRoot.Endpoint = "https://example.com/%2F"
	escapedRootFingerprint, err := escapedRoot.Fingerprint()
	require.NoError(t, err)
	assert.NotEqual(t, rootFingerprint, escapedRootFingerprint)

	upperZone := base
	upperZone.Endpoint = "http://[fe80::1%25ETH0]/v1"
	upperZoneFingerprint, err := upperZone.Fingerprint()
	require.NoError(t, err)
	lowerZone := base
	lowerZone.Endpoint = "http://[fe80::1%25eth0]/v1"
	lowerZoneFingerprint, err := lowerZone.Fingerprint()
	require.NoError(t, err)
	assert.NotEqual(t, upperZoneFingerprint, lowerZoneFingerprint)

	space := embedding.VectorSpaceIdentity{
		Provider: "synthetic", Model: "embed-1", ModelRevision: "2026-08",
		Dimension: 1_024, Normalization: "unit-length",
	}
	spaceFingerprint, err := space.Fingerprint()
	require.NoError(t, err)
	assert.NotEmpty(t, spaceFingerprint)
}

func TestEgressRejectsUnstableTextFields(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})
	_, err := (embedding.EgressIdentity{
		Purpose: embedding.EgressDocumentEmbedding, Provider: "synthetic",
		Endpoint: "https://example.com/v1", Model: "embed", ModelRevision: invalidUTF8,
	}).Fingerprint()
	require.ErrorContains(t, err, "invalid UTF-8")

	_, err = (embedding.VectorSpaceIdentity{
		Provider: "synthetic", Model: "embed", ModelRevision: "revision-1",
		Dimension: 1_024, Normalization: "unit\nlength",
	}).Fingerprint()
	require.ErrorContains(t, err, "control character")
}
