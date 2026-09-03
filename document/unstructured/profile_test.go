package unstructured

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bridge"
)

func TestReferenceProfileCanonicalizationPinsCompatibilityIdentity(t *testing.T) {
	profile, err := NewProfile(Config{
		DeploymentID:      "operator-unstructured-primary",
		RuntimeID:         "sha256:" + strings.Repeat("a", 64),
		CredentialBinding: "unstructured-api",
	})
	require.NoError(t, err)

	canonical, fingerprint, err := CanonicalProfile(profile)
	require.NoError(t, err)
	assert.Equal(t, fingerprint, profile.PolicyFingerprint)
	assert.JSONEq(t, `{
		"artifact_policy":{"allowed_roles":["structured_evidence"],"max_artifact_bytes":67108864,"max_artifacts":1},
		"bridge_contract":"docbank-rendition/v1",
		"contract_version":"unstructured-bridge-profile/v1",
		"credential_binding":"unstructured-api",
		"deployment_id":"operator-unstructured-primary",
		"disclosure":{"disclose_filename":true,"source":"exact_supplied_bytes"},
		"evidence_policy":{"max_provider_markdown_bytes":33554432,"max_total_result_bytes":134217728,"max_units":100000,"source_evidence_contract":"source-evidence/v1"},
		"input_kind":"original_file",
		"limits":{"max_document_bytes":104857600,"max_poll_attempts":300,"max_response_bytes":134217728,"poll_interval_millis":1000,"request_timeout_millis":30000,"total_timeout_millis":600000},
		"policy_fingerprint":"`+fingerprint+`",
		"runtime_id":"sha256:`+strings.Repeat("a", 64)+`",
			"supported_formats":[
				{"media_family":"ebook","media_type":"application/epub+zip","input_kind":"original_file"},
				{"media_family":"image","media_type":"image/jpeg","input_kind":"original_file"},
				{"media_family":"image","media_type":"image/png","input_kind":"original_file"},
				{"media_family":"mail","media_type":"message/rfc822","input_kind":"original_file"},
				{"media_family":"pdf","media_type":"application/pdf","input_kind":"original_file"},
				{"media_family":"presentation","media_type":"application/vnd.openxmlformats-officedocument.presentationml.presentation","input_kind":"original_file"},
				{"media_family":"spreadsheet","media_type":"application/vnd.oasis.opendocument.spreadsheet","input_kind":"original_file"},
				{"media_family":"spreadsheet","media_type":"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet","input_kind":"original_file"},
				{"media_family":"spreadsheet","media_type":"text/csv","input_kind":"original_file"},
				{"media_family":"structured","media_type":"application/xml","input_kind":"original_file"},
				{"media_family":"text","media_type":"text/markdown","input_kind":"original_file"},
				{"media_family":"text","media_type":"text/plain","input_kind":"original_file"}
		],
		"trust_boundary":"operator_network"
	}`, string(canonical))

	parsed, err := ParseProfile(canonical)
	require.NoError(t, err)
	assert.Equal(t, profile, parsed)
	_, err = ParseProfile(append(canonical, '\n'))
	require.ErrorContains(t, err, "not canonical")
}

func TestReferenceProfileAdvertisesOnlyEnforceableBroadSuppliedByteFormats(t *testing.T) {
	profile, err := NewProfile(Config{
		DeploymentID: "operator-unstructured-primary",
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
	assert.Equal(t, []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
		profile.ArtifactPolicy.AllowedRoles)
}

func TestReferenceProfileBuildsStandardBridgeAndRejectsDrift(t *testing.T) {
	profile, err := NewProfile(Config{
		DeploymentID:      "operator-unstructured-primary",
		RuntimeID:         "sha256:" + strings.Repeat("c", 64),
		CredentialBinding: "unstructured-api",
	})
	require.NoError(t, err)

	bridgeProfile, err := BridgeProfile(profile, "http://127.0.0.1:8421")
	require.NoError(t, err)
	assert.Equal(t, bridge.ContractVersion, profile.BridgeContract)
	assert.Equal(t, profile.CredentialBinding, bridgeProfile.SecretBinding)
	assert.Equal(t, document.RenditionTrustOperatorNetwork, bridgeProfile.Descriptor.TrustBoundary)
	assert.Equal(t, profile.PolicyFingerprint, bridgeProfile.Descriptor.PolicyFingerprint)
	assert.Equal(t, profile.SupportedFormats, bridgeProfile.Descriptor.SupportedFormats)
	assert.Equal(t, time.Duration(profile.Limits.RequestTimeoutMillis)*time.Millisecond,
		bridgeProfile.RequestTimeout)
	assert.Equal(t, time.Duration(profile.Limits.TotalTimeoutMillis)*time.Millisecond,
		bridgeProfile.TotalTimeout)
	assert.Equal(t, time.Duration(profile.Limits.PollIntervalMillis)*time.Millisecond,
		bridgeProfile.PollInterval)
	assert.Equal(t, profile.Limits.MaxPollAttempts, bridgeProfile.MaxPollAttempts)
	assert.Equal(t, profile.Limits.MaxResponseBytes, bridgeProfile.MaxResponseBytes)
	assert.Equal(t, profile.Limits.MaxDocumentBytes, bridgeProfile.MaxDocumentBytes)
	_, err = bridge.New(bridgeProfile, staticSecretResolver{}, http.DefaultClient)
	require.NoError(t, err)

	for _, test := range []struct {
		mutate func(*ProfileV1)
		want   string
	}{
		{mutate: func(value *ProfileV1) { value.RuntimeID = "sha256:" + strings.Repeat("d", 64) }, want: "policy fingerprint does not match"},
		{mutate: func(value *ProfileV1) { value.Limits.MaxDocumentBytes++ }, want: "limits differ"},
	} {
		drifted := profile
		test.mutate(&drifted)
		_, err := BridgeProfile(drifted, "http://127.0.0.1:8421")
		require.ErrorContains(t, err, test.want)
	}
}

func TestReferenceProfileRejectsUnpinnedIdentityAndUnboundedLimits(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		want   string
	}{
		{name: "deployment", config: Config{RuntimeID: "sha256:" + strings.Repeat("e", 64)}, want: "deployment ID"},
		{name: "runtime", config: Config{DeploymentID: "operator-unstructured-primary"}, want: "runtime ID"},
		{name: "credential", config: Config{DeploymentID: "operator-unstructured-primary", RuntimeID: "runtime-v1", CredentialBinding: "https://secret.invalid"}, want: "credential binding"},
		{name: "credential length", config: Config{DeploymentID: "operator-unstructured-primary", RuntimeID: "runtime-v1", CredentialBinding: strings.Repeat("c", 129)}, want: "credential binding"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewProfile(test.config)
			require.ErrorContains(t, err, test.want)
		})
	}

	profile, err := NewProfile(Config{DeploymentID: "operator-unstructured-primary", RuntimeID: "runtime-v1"})
	require.NoError(t, err)
	profile.Limits.MaxResponseBytes++
	profile.PolicyFingerprint = ""
	_, _, err = CanonicalProfile(profile)
	require.ErrorContains(t, err, "limits")
}

type staticSecretResolver struct{}

func (staticSecretResolver) ResolveSecret(context.Context, string) (string, error) {
	return "synthetic-secret", nil
}

var _ bridge.SecretResolver = staticSecretResolver{}
