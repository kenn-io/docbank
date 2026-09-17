package mistral

import (
	"bytes"
	"encoding/json/v2"
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
	want := `{"version":1,"provider":"mistral","endpoint":"https://api.eu.mistral.ai/v1/ocr","region":"eu","model":"mistral-ocr-4-0","retention":"zdr","training":"opted-out","max_document_bytes":1048576,"max_response_bytes":1048576,"max_units":500,"extract_header":true,"extract_footer":true,"normalization":{"version":3,"max_document_chars":100000,"max_unit_chars":1000000,"max_source_unit_bytes":4000000,"max_metadata_source_bytes":65536,"max_link_chars":2048,"max_chunk_runes":4000,"chunk_overlap":200,"max_chunks":20000},"format_authorities":[{"format_id":"pdf","unit_bound_method":"provider_request","request_fingerprint":"c5121284012927f702b20c744152a3afab0df72b76f07a6c065a39bb8b846b38","fixture_digest":"c35b21d6ca39aa7c"}]}`
	assert.Equal(t, want, string(encoded), "canonical policy JSON changed")
}

func TestPolicyRejectsUnknownPrivacyPostureAndMismatchedProbePolicy(t *testing.T) {
	normalization := testPolicy(t, 1<<20, 500).NormalizePolicy()
	_, err := NewPolicy(PolicyConfig{
		Region: defaultRegion, Model: "unavailable-model", Retention: "zdr", Training: "opted-out",
		MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 500,
		NormalizePolicy: normalization,
	})
	require.ErrorContains(t, err, "unavailable in region")

	_, err = NewPolicy(PolicyConfig{
		Region: defaultRegion, Model: defaultModel, Retention: "unknown", Training: "opted-out",
		MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 500,
		NormalizePolicy: normalization,
	})
	require.ErrorContains(t, err, "retention posture")

	_, err = NewPolicy(PolicyConfig{
		Region: defaultRegion, Model: defaultModel, Retention: "zdr", Training: "opted-out",
		MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 2,
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
	require.Error(t, err)

	compact, err := json.Marshal(manifest)
	require.NoError(t, err)
	duplicateResults := strings.Replace(string(compact), `"results":[`, `"results":[],"results":[`, 1)
	_, err = DecodeCapabilityManifest(strings.NewReader(duplicateResults))
	require.ErrorContains(t, err, `duplicate JSON object key "results"`)

	duplicateFormatID := strings.Replace(
		string(compact), `"format_id":"pdf"`, `"format_id":"docx","format_id":"pdf"`, 1,
	)
	_, err = DecodeCapabilityManifest(strings.NewReader(duplicateFormatID))
	require.ErrorContains(t, err, `duplicate JSON object key "format_id"`)

	caseFoldedFormatID := strings.Replace(
		string(compact), `"format_id":"pdf"`, `"format_id":"docx","FORMAT_ID":"pdf"`, 1,
	)
	_, err = DecodeCapabilityManifest(strings.NewReader(caseFoldedFormatID))
	require.ErrorContains(t, err, `JSON object key "FORMAT_ID" must use lowercase ASCII`)

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

func TestCapabilityManifestRejectsInvalidAuthorityEvidence(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	tests := []struct {
		name   string
		mutate func(*CapabilityManifest)
		want   string
	}{
		{name: "old schema", mutate: func(manifest *CapabilityManifest) { manifest.SchemaVersion-- }, want: "schema must be"},
		{name: "old fixture contract", mutate: func(manifest *CapabilityManifest) { manifest.ProbeFixtureContract-- }, want: "fixture contract must be"},
		{name: "future observation", mutate: func(manifest *CapabilityManifest) { manifest.ObservedOn = "2999-01-01" }, want: "invalid observation date"},
		{name: "target mismatch", mutate: func(manifest *CapabilityManifest) { manifest.Endpoint = "https://example.invalid/v1/ocr" }, want: "not pinned"},
		{name: "unit limit", mutate: func(manifest *CapabilityManifest) { manifest.MaxUnits = 1 }, want: "invalid unit limit"},
		{name: "partial matrix", mutate: func(manifest *CapabilityManifest) { manifest.Results = manifest.Results[:len(manifest.Results)-1] }, want: "results"},
		{name: "reordered matrix", mutate: func(manifest *CapabilityManifest) {
			manifest.Results[0], manifest.Results[1] = manifest.Results[1], manifest.Results[0]
		}, want: "does not match"},
		{name: "fixture count mismatch", mutate: func(manifest *CapabilityManifest) { manifest.Results[0].FixtureUnits++ }, want: "provider-request bound evidence"},
		{name: "zero requested units", mutate: func(manifest *CapabilityManifest) { manifest.Results[0].BoundRequestedUnits = 0 }, want: "provider-request bound evidence"},
		{name: "request not below fixture", mutate: func(manifest *CapabilityManifest) {
			manifest.Results[0].BoundRequestedUnits = manifest.Results[0].FixtureUnits
		}, want: "provider-request bound evidence"},
		{name: "fixture reaches authority", mutate: func(manifest *CapabilityManifest) {
			manifest.Results[0].FixtureUnits = manifest.MaxUnits
			manifest.Results[0].UnitCount = manifest.MaxUnits
			manifest.Results[0].UnitsProcessed = manifest.MaxUnits
		}, want: "provider-request bound evidence"},
		{name: "processed bound mismatch", mutate: func(manifest *CapabilityManifest) { manifest.Results[0].BoundUnitsProcessed++ }, want: "provider-request bound evidence"},
		{name: "local exact without claim", mutate: func(manifest *CapabilityManifest) {
			manifest.Results[1].UnitBoundMethod = UnitBoundLocalExact
			manifest.Results[1].LocalUnits = manifest.Results[1].UnitsProcessed
		}, want: "local-exact bound evidence"},
		{name: "local counted without claim", mutate: func(manifest *CapabilityManifest) {
			manifest.Results[1].UnitBoundMethod = UnitBoundLocalCounted
			manifest.Results[1].LocalUnits = manifest.Results[1].UnitsProcessed
		}, want: "local-counted bound evidence"},
		{name: "provider bound without claim", mutate: func(manifest *CapabilityManifest) {
			manifest.Results[1].UnitBoundMethod = UnitBoundProviderRequest
			manifest.Results[1].FixtureUnits = 2
			manifest.Results[1].UnitCount = 2
			manifest.Results[1].UnitsProcessed = 2
			manifest.Results[1].BoundRequestedUnits = 1
			manifest.Results[1].BoundUnitsProcessed = 1
		}, want: "provider-request bound evidence"},
		{name: "none with observations", mutate: func(manifest *CapabilityManifest) {
			manifest.Results[0].UnitBoundMethod = UnitBoundNone
			manifest.Results[0].ReasonCode = reasonBoundUnitsMismatch
		}, want: "observations without a unit bound"},
		{name: "unexplained PPTX bound", mutate: func(manifest *CapabilityManifest) {
			for index := range manifest.Results {
				if manifest.Results[index].FormatID == "pptx" {
					manifest.Results[index].ReasonCode = ""
				}
			}
		}, want: "does not explain its unverified bound"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := syntheticManifest(t, policy, true)
			test.mutate(&manifest)
			require.ErrorContains(t, manifest.ValidateComplete(), test.want)
		})
	}
}

func TestLocalCountedManifestValidation(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := textAuthorityManifest(t, policy, "json")
	for index := range manifest.Results {
		if manifest.Results[index].FormatID == "json" {
			manifest.Results[index].UnitCount = 2
			manifest.Results[index].UnitsProcessed = 2
			manifest.Results[index].LocalUnits = 3
		}
	}
	require.NoError(t, manifest.ValidateComplete())

	for _, test := range []struct {
		name   string
		mutate func(*CapabilityManifest)
	}{
		{name: "zero provider units", mutate: func(value *CapabilityManifest) {
			value.Results[17].UnitCount = 0
		}},
		{name: "provider fields differ", mutate: func(value *CapabilityManifest) {
			value.Results[17].UnitsProcessed = 1
		}},
		{name: "provider units exceed limit", mutate: func(value *CapabilityManifest) {
			value.Results[17].UnitCount = value.MaxUnits + 1
			value.Results[17].UnitsProcessed = value.MaxUnits + 1
		}},
		{name: "local units exceed limit", mutate: func(value *CapabilityManifest) {
			for index := range value.Results {
				if value.Results[index].FormatID == "json" {
					value.Results[index].LocalUnits = value.MaxUnits + 1
				}
			}
		}},
		{name: "zero local units", mutate: func(value *CapabilityManifest) {
			for index := range value.Results {
				if value.Results[index].FormatID == "json" {
					value.Results[index].LocalUnits = 0
				}
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := manifest
			candidate.Results = append([]CapabilityResult(nil), manifest.Results...)
			test.mutate(&candidate)
			require.Error(t, candidate.ValidateComplete())
		})
	}
}

func TestProbeReasonConstants(t *testing.T) {
	assert.Equal(t, "bound_request_failed", reasonBoundRequestFailed)
	assert.Equal(t, "bound_units_mismatch", reasonBoundUnitsMismatch)
}

func TestTextManifestRequiresFreshLocalCountedEvidence(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)
	_, err := policy.Authorize(manifest, "json")
	require.ErrorContains(t, err, "no enforceable unit bound")

	for index := range manifest.Results {
		if manifest.Results[index].FormatID == "json" {
			manifest.Results[index].ReasonCode = ""
			manifest.Results[index].UnitBoundMethod = UnitBoundLocalCounted
			manifest.Results[index].LocalUnits = 1
		}
	}
	require.NoError(t, manifest.ValidateComplete())
	_, err = policy.Authorize(manifest, "json")
	require.NoError(t, err)

	stale := manifest
	for index := range stale.Results {
		if stale.Results[index].FormatID == "json" {
			stale.Results[index].UnitBoundMethod = UnitBoundNone
			stale.Results[index].ReasonCode = reasonBoundUnitsMismatch
			stale.Results[index].LocalUnits = 0
		}
	}
	require.NoError(t, stale.ValidateComplete())
	_, err = policy.Authorize(stale, "json")
	require.ErrorContains(t, err, "no enforceable unit bound")
	t.Logf("stale_manifest_error=%v", err)
}

func TestPolicyRejectsIdentityBeyondManifestAuthority(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifestPolicy := testPolicy(t, 1<<20, 9)
	manifest := syntheticManifest(t, manifestPolicy, true)

	_, err := policy.CanonicalJSON(manifest)
	require.ErrorContains(t, err, "unit limit 10 exceeds capability manifest authority 9")
	_, err = policy.Authorize(manifest, "pdf")
	require.ErrorContains(t, err, "unit limit 10 exceeds capability manifest authority 9")
	assert.NotContains(t, err.Error(), "run the authenticated capability probe")

	_, err = policy.Authorize(syntheticManifest(t, policy, true), "unknown")
	require.ErrorContains(t, err, `format "unknown" is unknown`)
	assert.NotContains(t, err.Error(), "run the authenticated capability probe")
}
