package tika

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bridge"
)

func TestReferenceProfileCanonicalizesCompleteCompatibilityIdentity(t *testing.T) {
	profile, err := NewProfile(Config{
		DeploymentID:      "operator-tika-primary",
		RuntimeID:         "sha256:" + strings.Repeat("a", 64),
		CredentialBinding: "tika-api",
	})
	require.NoError(t, err)

	canonical, fingerprint, err := CanonicalProfile(profile)
	require.NoError(t, err)
	assert.Equal(t, profile.PolicyFingerprint, fingerprint)
	parsed, err := ParseProfile(canonical)
	require.NoError(t, err)
	assert.Equal(t, profile, parsed)

	reordered := profile
	reordered.PolicyFingerprint = ""
	reordered.SupportedFormats = slices.Clone(profile.SupportedFormats)
	slices.Reverse(reordered.SupportedFormats)
	reorderedJSON, reorderedFingerprint, err := CanonicalProfile(reordered)
	require.NoError(t, err)
	assert.JSONEq(t, string(canonical), string(reorderedJSON))
	assert.Equal(t, fingerprint, reorderedFingerprint)

	var wire map[string]any
	require.NoError(t, json.Unmarshal(canonical, &wire))
	assert.ElementsMatch(t, []string{
		"artifact_policy", "bridge_contract", "contract_version", "credential_binding",
		"deployment_id", "disclosure", "evidence_policy", "input_kind", "limits",
		"policy_fingerprint", "reference_policy", "runtime_id", "supported_formats",
		"trust_boundary",
	}, mapKeys(wire))
	_, err = ParseProfile(append(canonical, '\n'))
	require.ErrorContains(t, err, "not canonical")
}

func TestReferenceProfileRestrictsBroadFormatsToSuppliedBytesAndRefusesReferences(t *testing.T) {
	profile, err := NewProfile(Config{
		DeploymentID: "operator-tika-primary",
		RuntimeID:    "sha256:" + strings.Repeat("b", 64),
	})
	require.NoError(t, err)

	assert.Equal(t, []document.RenditionFormatCapability{
		{MediaFamily: "ebook", MediaType: "application/epub+zip", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/png", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "mail", MediaType: "message/rfc822", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "presentation", MediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "spreadsheet", MediaType: "application/vnd.oasis.opendocument.spreadsheet", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "spreadsheet", MediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "spreadsheet", MediaType: "text/csv", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "structured", MediaType: "application/xml", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "text", MediaType: "text/markdown", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "text", MediaType: "text/plain", InputKind: document.RenditionInputOriginalFile},
	}, profile.SupportedFormats)
	assert.Equal(t, "exact_supplied_bytes", profile.Disclosure.Source)
	assert.True(t, profile.Disclosure.DiscloseFilename)
	assert.Equal(t, "refuse", profile.ReferencePolicy.EmbeddedReferenceFetch)
	assert.Equal(t, "refuse", profile.ReferencePolicy.ExternalReferenceFetch)
	assert.Equal(t, "pinned_audited_adapter_runtime", profile.ReferencePolicy.EnforcementBoundary)
	assert.Equal(t, []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
		profile.ArtifactPolicy.AllowedRoles)
}

func TestReferenceProfileBuildsStandardBridgeAndRejectsIdentityOrLimitDrift(t *testing.T) {
	profile, err := NewProfile(Config{
		DeploymentID:      "operator-tika-primary",
		RuntimeID:         "sha256:" + strings.Repeat("c", 64),
		CredentialBinding: "tika-api",
	})
	require.NoError(t, err)

	bridgeProfile, err := BridgeProfile(profile, "http://127.0.0.1:9998")
	require.NoError(t, err)
	assert.Equal(t, bridge.ContractVersion, profile.BridgeContract)
	assert.Equal(t, profile.CredentialBinding, bridgeProfile.SecretBinding)
	assert.Equal(t, document.RenditionTrustOperatorNetwork, bridgeProfile.Descriptor.TrustBoundary)
	assert.Equal(t, profile.PolicyFingerprint, bridgeProfile.Descriptor.PolicyFingerprint)
	assert.Equal(t, profile.SupportedFormats, bridgeProfile.Descriptor.SupportedFormats)
	assert.True(t, bridgeProfile.Descriptor.ReturnsMarkdown)
	assert.True(t, bridgeProfile.Descriptor.ReturnsStructured)
	assert.Equal(t, profile.ArtifactPolicy.AllowedRoles, bridgeProfile.Descriptor.ArtifactRoles)
	assert.Equal(t, profile.Limits.MaxDocumentBytes, bridgeProfile.MaxSourceBytes)
	assert.Equal(t, profile.EvidencePolicy.MaxProviderMarkdownBytes, bridgeProfile.MaxProviderMarkdownBytes)
	assert.Equal(t, int(profile.ArtifactPolicy.MaxArtifactBytes), bridgeProfile.MaxArtifactBytes)
	assert.Equal(t, profile.ArtifactPolicy.MaxArtifacts, bridgeProfile.MaxArtifacts)
	assert.Equal(t, profile.EvidencePolicy.MaxTotalResultBytes, bridgeProfile.MaxTotalResultBytes)
	assert.Equal(t, profile.EvidencePolicy.MaxUnits, bridgeProfile.MaxEvidenceUnits)
	_, err = bridge.New(bridgeProfile, staticSecretResolver{}, http.DefaultClient)
	require.NoError(t, err)

	credentialless, err := NewProfile(Config{
		DeploymentID: "operator-tika-no-auth", RuntimeID: "sha256:" + strings.Repeat("e", 64),
	})
	require.NoError(t, err)
	credentiallessBridge, err := BridgeProfile(credentialless, "http://127.0.0.1:9998")
	require.NoError(t, err)
	_, err = bridge.New(credentiallessBridge, nil, http.DefaultClient)
	require.NoError(t, err)

	for _, test := range []struct {
		mutate func(*ProfileV1)
		want   string
	}{
		{mutate: func(value *ProfileV1) { value.RuntimeID = "sha256:" + strings.Repeat("d", 64) }, want: "policy fingerprint does not match"},
		{mutate: func(value *ProfileV1) { value.Limits.MaxDocumentBytes++ }, want: "limits differ"},
		{mutate: func(value *ProfileV1) { value.ReferencePolicy.ExternalReferenceFetch = "allow" }, want: "reference fetching must be refused"},
	} {
		drifted := profile
		test.mutate(&drifted)
		_, err := BridgeProfile(drifted, "http://127.0.0.1:9998")
		require.ErrorContains(t, err, test.want)
	}
}

func TestReferenceProfileRejectsUnpinnedIdentityAndRecanonicalizedPolicyDrift(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		want   string
	}{
		{name: "deployment", config: Config{RuntimeID: "runtime-v1"}, want: "deployment ID"},
		{name: "runtime", config: Config{DeploymentID: "operator-tika-primary"}, want: "runtime ID"},
		{name: "credential", config: Config{DeploymentID: "operator-tika-primary", RuntimeID: "runtime-v1", CredentialBinding: "https://secret.invalid"}, want: "credential binding"},
		{name: "credential exceeds bridge bound", config: Config{DeploymentID: "operator-tika-primary", RuntimeID: "runtime-v1", CredentialBinding: strings.Repeat("a", 129)}, want: "credential binding"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewProfile(test.config)
			require.ErrorContains(t, err, test.want)
		})
	}

	profile, err := NewProfile(Config{DeploymentID: "operator-tika-primary", RuntimeID: "runtime-v1"})
	require.NoError(t, err)
	profile.PolicyFingerprint = ""
	profile.Limits.MaxResponseBytes++
	_, _, err = CanonicalProfile(profile)
	require.ErrorContains(t, err, "limits differ")

	profile, err = NewProfile(Config{DeploymentID: "operator-tika-primary", RuntimeID: "runtime-v1"})
	require.NoError(t, err)
	profile.PolicyFingerprint = ""
	profile.ReferencePolicy.EmbeddedReferenceFetch = "allow"
	_, _, err = CanonicalProfile(profile)
	require.ErrorContains(t, err, "reference fetching must be refused")

	boundary, err := NewProfile(Config{
		DeploymentID: "operator-tika-primary", RuntimeID: "runtime-v1",
		CredentialBinding: strings.Repeat("a", maxCredentialBindingBytes),
	})
	require.NoError(t, err)
	bridgeProfile, err := BridgeProfile(boundary, "http://127.0.0.1:9998")
	require.NoError(t, err)
	_, err = bridge.New(bridgeProfile, staticSecretResolver{}, http.DefaultClient)
	require.NoError(t, err, "profile and bridge credential bounds must stay aligned")
}

type staticSecretResolver struct{}

func (staticSecretResolver) ResolveSecret(context.Context, string) (string, error) {
	return "synthetic-secret", nil
}

func mapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	return keys
}

var _ bridge.SecretResolver = staticSecretResolver{}
