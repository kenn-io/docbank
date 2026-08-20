package voyage_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/document/voyage/voyagetest"
)

func testPolicy(t *testing.T) voyage.Policy {
	t.Helper()
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Media: media.DefaultPolicy()})
	require.NoError(t, err)
	return policy
}

func TestNewPolicyPinsProviderAndAppliesDefaults(t *testing.T) {
	assert := assert.New(t)
	policy := testPolicy(t)
	values := policy.Values()
	assert.Equal("voyage", values.Provider)
	assert.Equal(voyage.DefaultEndpoint, values.Endpoint)
	assert.Equal(voyage.DefaultModel, values.Model)
	assert.Equal(voyage.DefaultDimension, values.Dimension)
	assert.Equal(voyage.MaxBatchItems, values.MaxBatchItems)
	assert.Equal(voyage.MaxRequestBytes, values.MaxRequestBytes)
	assert.Equal(voyage.MaxResponseBytes, values.MaxResponseBytes)
	assert.Equal(media.DefaultPolicy(), policy.MediaPolicy())
}

func TestNewPolicyRejectsUnpinnedTargetsAndInvalidBounds(t *testing.T) {
	tests := []struct {
		name   string
		config voyage.PolicyConfig
		want   string
	}{
		{name: "model", config: voyage.PolicyConfig{Model: "voyage-4", Media: media.DefaultPolicy()}, want: "not pinned"},
		{name: "dimension", config: voyage.PolicyConfig{Dimension: 512, Media: media.DefaultPolicy()}, want: "not pinned"},
		{name: "batch above cap", config: voyage.PolicyConfig{MaxBatchItems: voyage.MaxBatchItems + 1, Media: media.DefaultPolicy()}, want: "request bounds"},
		{name: "batch negative", config: voyage.PolicyConfig{MaxBatchItems: -1, Media: media.DefaultPolicy()}, want: "request bounds"},
		{name: "request bytes above cap", config: voyage.PolicyConfig{MaxRequestBytes: voyage.MaxRequestBytes + 1, Media: media.DefaultPolicy()}, want: "request bounds"},
		{name: "response bytes above cap", config: voyage.PolicyConfig{MaxResponseBytes: voyage.MaxResponseBytes + 1, Media: media.DefaultPolicy()}, want: "request bounds"},
		{name: "media admits nothing", config: voyage.PolicyConfig{}, want: "media bounds"},
		{name: "media above hard cap", config: voyage.PolicyConfig{Media: media.Policy{MaxBytes: media.MaxBytes + 1, AllowStill: true}}, want: "media bounds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := voyage.NewPolicy(tt.config)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestPolicyFingerprintIsStableAndTracksAuthorities(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	policy := testPolicy(t)
	full, err := voyagetest.SyntheticManifest(policy)
	require.NoError(err)
	partial, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityImagePNG, voyage.CapabilityQueryText)
	require.NoError(err)

	fullFingerprint, err := policy.Fingerprint(full)
	require.NoError(err)
	again, err := policy.Fingerprint(full)
	require.NoError(err)
	assert.Equal(fullFingerprint, again)
	assert.Len(fullFingerprint, 64)

	partialFingerprint, err := policy.Fingerprint(partial)
	require.NoError(err)
	assert.NotEqual(fullFingerprint, partialFingerprint, "authorized capabilities are part of the identity")

	canonical, err := policy.CanonicalJSON(partial)
	require.NoError(err)
	var decoded map[string]any
	require.NoError(json.Unmarshal(canonical, &decoded))
	authorities, ok := decoded["capability_authorities"].([]any)
	require.True(ok)
	assert.Len(authorities, 2)
	assert.Contains(string(canonical), `"provider":"voyage"`)
	assert.Contains(string(canonical), `"media":`)

	stricter, err := voyage.NewPolicy(voyage.PolicyConfig{Media: media.Policy{MaxBytes: 1 << 20, AllowStill: true}})
	require.NoError(err)
	stricterFingerprint, err := stricter.Fingerprint(full)
	require.NoError(err)
	assert.NotEqual(fullFingerprint, stricterFingerprint, "media bounds are part of the identity")

	var zero voyage.Policy
	_, err = zero.Fingerprint(full)
	require.ErrorContains(err, "use NewPolicy")
}

func TestAuthorizeFailsClosed(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	policy := testPolicy(t)
	manifest, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityImagePNG)
	require.NoError(err)

	authorization, err := policy.Authorize(manifest, voyage.CapabilityImagePNG)
	require.NoError(err)
	assert.Equal(voyage.CapabilityImagePNG, authorization.Capability().ID)
	fingerprint, err := policy.Fingerprint(manifest)
	require.NoError(err)
	assert.Equal(fingerprint, authorization.PolicyFingerprint())

	_, err = policy.Authorize(manifest, voyage.CapabilityImageJPEG)
	require.ErrorContains(err, "did not pass")
	_, err = policy.Authorize(manifest, "image_heic")
	require.ErrorContains(err, "unknown")
	_, err = policy.Authorize(voyage.CapabilityManifest{}, voyage.CapabilityImagePNG)
	require.ErrorContains(err, "authorized upload authority")

	retargeted := manifest
	retargeted.Results = append([]voyage.CapabilityResult(nil), manifest.Results...)
	retargeted.Results[1].RequestFingerprint = strings.Repeat("0", 64)
	_, err = policy.Authorize(retargeted, voyage.CapabilityImagePNG)
	require.ErrorContains(err, "different request policy")

	smallerBatch := manifest
	smallerBatch.MaxBatchItems = 8
	_, err = policy.Authorize(smallerBatch, voyage.CapabilityImagePNG)
	require.ErrorContains(err, "exceeds capability manifest authority")

	var zero voyage.Policy
	_, err = zero.Authorize(manifest, voyage.CapabilityImagePNG)
	require.ErrorContains(err, "use NewPolicy")

	all, err := policy.AuthorizeAll(manifest)
	require.NoError(err)
	require.Len(all, 1)
	assert.Equal(voyage.CapabilityImagePNG, all[0].Capability().ID)
}

func TestManifestValidationAndStrictDecoding(t *testing.T) {
	policy := testPolicy(t)
	manifest, err := voyagetest.SyntheticManifest(policy)
	require.NoError(t, err)

	var encoded bytes.Buffer
	require.NoError(t, voyage.EncodeCapabilityManifest(&encoded, manifest))
	decoded, err := voyage.DecodeCapabilityManifest(bytes.NewReader(encoded.Bytes()))
	require.NoError(t, err)
	require.Equal(t, manifest, decoded)

	mutate := func(name string, mutate func(*voyage.CapabilityManifest), want string) {
		t.Run(name, func(t *testing.T) {
			candidate := manifest
			candidate.Results = append([]voyage.CapabilityResult(nil), manifest.Results...)
			mutate(&candidate)
			require.ErrorContains(t, candidate.ValidateComplete(), want)
		})
	}
	mutate("schema", func(m *voyage.CapabilityManifest) { m.SchemaVersion = 99 }, "schema")
	mutate("fixture contract", func(m *voyage.CapabilityManifest) { m.ProbeFixtureContract = 99 }, "fixture contract")
	mutate("future date", func(m *voyage.CapabilityManifest) { m.ObservedOn = "2999-01-01" }, "observation date")
	mutate("bad date", func(m *voyage.CapabilityManifest) { m.ObservedOn = "yesterday" }, "observation date")
	mutate("endpoint", func(m *voyage.CapabilityManifest) { m.Endpoint = "https://api.example.test/v1" }, "not pinned")
	mutate("model", func(m *voyage.CapabilityManifest) { m.Model = "other" }, "not pinned")
	mutate("batch", func(m *voyage.CapabilityManifest) { m.MaxBatchItems = 0 }, "batch limit")
	mutate("count", func(m *voyage.CapabilityManifest) { m.Results = m.Results[:3] }, "results, want")
	mutate("order", func(m *voyage.CapabilityManifest) {
		m.Results[0], m.Results[1] = m.Results[1], m.Results[0]
	}, "must be")
	mutate("status", func(m *voyage.CapabilityManifest) { m.Results[0].Status = "maybe" }, "invalid status")
	mutate("digest", func(m *voyage.CapabilityManifest) { m.Results[0].FixtureDigest = "XYZ" }, "fixture digest")
	mutate("fingerprint", func(m *voyage.CapabilityManifest) { m.Results[0].RequestFingerprint = "abc" }, "request fingerprint")
	mutate("unscrubbed failure", func(m *voyage.CapabilityManifest) {
		tokens := int64(5)
		m.Results[0].Status = voyage.ProbeStatusFailed
		m.Results[0].ReasonCode = voyage.ReasonTransientExhausted
		m.Results[0].TotalTokens = &tokens
	}, "not scrubbed")
	mutate("failure without reason", func(m *voyage.CapabilityManifest) {
		m.Results[0].Status = voyage.ProbeStatusRejected
	}, "not scrubbed")
	mutate("passing with reason", func(m *voyage.CapabilityManifest) {
		m.Results[0].ReasonCode = voyage.ReasonProviderRejected
	}, "inconsistent")

	t.Run("decode rejects duplicate keys", func(t *testing.T) {
		text := strings.Replace(encoded.String(), `"schema_version": 2,`, `"schema_version": 2, "schema_version": 2,`, 1)
		_, err := voyage.DecodeCapabilityManifest(strings.NewReader(text))
		require.ErrorContains(t, err, "duplicate")
	})
	t.Run("decode rejects unknown fields", func(t *testing.T) {
		text := strings.Replace(encoded.String(), `"schema_version": 2,`, `"schema_version": 2, "vectors": [],`, 1)
		_, err := voyage.DecodeCapabilityManifest(strings.NewReader(text))
		require.ErrorContains(t, err, "unknown field")
	})
	t.Run("decode rejects trailing JSON", func(t *testing.T) {
		_, err := voyage.DecodeCapabilityManifest(strings.NewReader(encoded.String() + "{}"))
		require.ErrorContains(t, err, "trailing")
	})
	t.Run("decode rejects oversized input", func(t *testing.T) {
		_, err := voyage.DecodeCapabilityManifest(strings.NewReader(strings.Repeat(" ", 2<<20)))
		require.ErrorContains(t, err, "too large")
	})
	t.Run("encode rejects invalid manifest", func(t *testing.T) {
		require.Error(t, voyage.EncodeCapabilityManifest(&bytes.Buffer{}, voyage.CapabilityManifest{}))
	})
}
