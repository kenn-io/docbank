package mistral

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyAuthorizesOnlyProbeTestedBounds(t *testing.T) {
	policy := testPolicy(t, 1<<20, 500)
	manifest := syntheticManifest(t, policy, true)

	authorization, err := policy.Authorize(manifest, "pdf")
	require.NoError(t, err)
	assert.Equal(t, "pdf", authorization.Format().ID)
	fingerprint, err := policy.Fingerprint(manifest)
	require.NoError(t, err)
	assert.Equal(t, fingerprint, authorization.PolicyFingerprint())

	_, err = policy.Authorize(manifest, "docx")
	require.ErrorContains(t, err, "run the authenticated capability probe")

	manifest = syntheticManifest(t, policy, false)
	_, err = policy.Authorize(manifest, "pdf")
	require.ErrorContains(t, err, "no enforceable unit bound")
}

func TestPolicyFingerprintExcludesObservationDate(t *testing.T) {
	policy := testPolicy(t, 1<<20, 500)
	first := syntheticManifest(t, policy, true)
	second := first
	second.ObservedOn = "2026-08-17"

	firstFingerprint, err := policy.Fingerprint(first)
	require.NoError(t, err)
	secondFingerprint, err := policy.Fingerprint(second)
	require.NoError(t, err)
	assert.Equal(t, firstFingerprint, secondFingerprint)

	encoded, err := policy.CanonicalJSON(first)
	require.NoError(t, err)
	want := `{"version":1,"provider":"mistral","endpoint":"https://api.mistral.ai/v1/ocr","region":"eu","model":"mistral-ocr-4-0","retention":"zdr","training":"opted-out","max_document_bytes":1048576,"max_response_bytes":1048576,"max_units":500,"extract_header":true,"extract_footer":true,"normalization":{"version":2,"max_document_chars":100000,"max_unit_chars":1000000,"max_source_unit_bytes":4000000,"max_metadata_source_bytes":65536,"max_link_chars":2048,"max_chunk_runes":4000,"chunk_overlap":200,"max_chunks":20000},"format_authorities":[{"format_id":"pdf","unit_bound_method":"provider_request","request_fingerprint":"b93829e3f4ccc8e890b4a999ae155cfa30b40e64def51115bc4b809b5623e788","fixture_digest":"c35b21d6ca39aa7c"}]}`
	if string(encoded) != want {
		t.Fatalf("canonical policy JSON changed:\n got: %s\nwant: %s", encoded, want)
	}
}

func TestPolicyRejectsUnknownPrivacyPostureAndMismatchedProbePolicy(t *testing.T) {
	normalization := testPolicy(t, 1<<20, 500).NormalizePolicy()
	_, err := NewPolicy(PolicyConfig{
		Region: defaultRegion, Model: defaultModel, Retention: "unknown", Training: "opted-out",
		MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 500,
		NormalizePolicy: normalization,
	})
	require.ErrorContains(t, err, "retention posture")

	_, err = NewPolicy(PolicyConfig{
		Region: defaultRegion, Model: defaultModel, Retention: "zdr", Training: "opted-out",
		MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 1,
		NormalizePolicy: normalization,
	})
	require.ErrorContains(t, err, "processing bounds")

	policy := testPolicy(t, 1<<20, 500)
	manifest := syntheticManifest(t, policy, true)
	manifest.Results[0].RequestFingerprint = strings.Repeat("0", 64)
	require.NoError(t, manifest.ValidateComplete())
	_, err = policy.Authorize(manifest, "pdf")
	require.ErrorContains(t, err, "different request policy")
}

func TestCapabilityManifestDecodingIsStrictAndBounded(t *testing.T) {
	policy := testPolicy(t, 1<<20, 500)
	manifest := syntheticManifest(t, policy, true)
	var encoded bytes.Buffer
	require.NoError(t, EncodeCapabilityManifest(&encoded, manifest))
	decoded, err := DecodeCapabilityManifest(bytes.NewReader(encoded.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, manifest, decoded)

	var object map[string]any
	require.NoError(t, json.Unmarshal(encoded.Bytes(), &object))
	object["unexpected"] = true
	unknown, err := json.Marshal(object)
	require.NoError(t, err)
	_, err = DecodeCapabilityManifest(bytes.NewReader(unknown))
	require.ErrorContains(t, err, "unknown field")

	_, err = DecodeCapabilityManifest(strings.NewReader(strings.Repeat("x", int(maxManifestBytes)+1)))
	require.ErrorContains(t, err, "too large")

	unsafe := manifest
	unsafe.Results = append([]CapabilityResult(nil), manifest.Results...)
	unsafe.Results[1].Status = ProbeStatusFailed
	unsafe.Results[1].ReasonCode = "operator supplied private detail"
	unsafe.Results[1].ReturnedModel = ""
	unsafe.Results[1].UnitCount = 0
	unsafe.Results[1].UnitsProcessed = 0
	require.ErrorContains(t, unsafe.ValidateComplete(), "not scrubbed")
}
