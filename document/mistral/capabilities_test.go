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

func TestDOCXRouteAuthority(t *testing.T) {
	unconfigured := testPolicy(t, 1<<20, 11)
	manifest := syntheticManifest(t, unconfigured, true)
	_, err := unconfigured.Authorize(manifest, "docx")
	require.ErrorContains(t, err, "no enforceable unit bound")

	renderPolicy := testRenderPDFPolicy(t, testMultipagePDF(1), nil)
	configured := testPolicyWithRenderPDF(t, renderPolicy, 1<<20, 11)
	configuredManifest := syntheticManifest(t, configured, true)
	authorization, err := configured.Authorize(configuredManifest, "docx")
	require.NoError(t, err)
	assert.Equal(t, "docx", authorization.Format().ID)
	assert.Equal(t, UnitBoundLocalExact, authorization.method)

	withoutPDF := syntheticManifest(t, configured, false)
	_, err = configured.Authorize(withoutPDF, "docx")
	require.ErrorContains(t, err, "no enforceable unit bound")

	wrongOptions := configuredManifest
	wrongOptions.Results = append([]CapabilityResult(nil), configuredManifest.Results...)
	for index := range wrongOptions.Results {
		if wrongOptions.Results[index].FormatID == formatIDPDF {
			wrongOptions.Results[index].RequestFingerprint = strings.Repeat("0", 64)
		}
	}
	_, err = configured.Authorize(wrongOptions, "docx")
	require.ErrorContains(t, err, "different request policy")
}

func TestRenderRouteNegativeSpace(t *testing.T) {
	configured := testPolicyWithRenderPDF(t, testRenderPDFPolicy(t, testMultipagePDF(1), nil), 1<<20, 11)
	for _, formatID := range []string{"docx", "doc", "odt", "rtf", "ppt", "xls", "ods", "xlsx"} {
		assert.True(t, configured.rendersToPDF(formatID), formatID)
	}
	for _, formatID := range []string{"pdf", "pptx", "txt", "numbers"} {
		assert.False(t, configured.rendersToPDF(formatID), formatID)
	}
	unconfigured := testPolicy(t, 1<<20, 11)
	assert.False(t, unconfigured.rendersToPDF("docx"))
	assert.False(t, unconfigured.rendersToPDF("xlsx"))

	manifest := syntheticManifest(t, configured, true)
	for _, formatID := range []string{"docx", "doc", "odt", "rtf", "ppt", "xls", "ods", "xlsx"} {
		authorization, err := configured.Authorize(manifest, formatID)
		require.NoError(t, err, formatID)
		assert.Equal(t, UnitBoundLocalExact, authorization.method, formatID)
	}
	_, err := unconfigured.Authorize(syntheticManifest(t, unconfigured, true), "xlsx")
	require.ErrorContains(t, err, "no enforceable unit bound")
}

func TestDOCXPolicyIdentity(t *testing.T) {
	base := testPolicy(t, 1<<20, 11)
	manifest := syntheticManifest(t, base, true)
	baseFingerprint, err := base.Fingerprint(manifest)
	require.NoError(t, err)

	renderPolicy := testRenderPDFPolicy(t, testMultipagePDF(1), nil)
	configured := testPolicyWithRenderPDF(t, renderPolicy, 1<<20, 11)
	configuredManifest := syntheticManifest(t, configured, true)
	configuredFingerprint, err := configured.Fingerprint(configuredManifest)
	require.NoError(t, err)
	assert.NotEqual(t, baseFingerprint, configuredFingerprint)
	assert.Empty(t, base.Values().RenderPDFFingerprint)
	assert.Equal(t, renderPolicy.Fingerprint(), configured.Values().RenderPDFFingerprint)

	authorization, err := configured.Authorize(configuredManifest, "docx")
	require.NoError(t, err)
	_, err = baseClientForTest(t, base).Process(t.Context(),
		prepareTestDOCX(t, configured, loadDOCXFixture(t, "explicit-breaks.docx")), authorization)
	require.ErrorContains(t, err, "different policy")
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
		{name: "provider response without claim", mutate: func(manifest *CapabilityManifest) {
			manifest.Results[1].UnitBoundMethod = UnitBoundProviderResponse
			manifest.Results[1].LocalUnits = manifest.Results[1].UnitsProcessed
		}, want: "provider-response evidence"},
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

func TestProviderResponseManifestValidation(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	for _, test := range []struct {
		name   string
		mutate func(*CapabilityResult)
		want   string
	}{
		{name: "zero provider units", mutate: func(value *CapabilityResult) {
			value.UnitCount = 0
		}, want: "passing result \"json\" is incomplete"},
		{name: "provider fields differ", mutate: func(value *CapabilityResult) {
			value.UnitsProcessed++
		}, want: "passing result \"json\" is incomplete"},
		{name: "provider units exceed limit", mutate: func(value *CapabilityResult) {
			value.UnitCount = policy.values.MaxUnits + 1
			value.UnitsProcessed = value.UnitCount
		}, want: "invalid provider-response evidence"},
		{name: "unexpected local units", mutate: func(value *CapabilityResult) {
			value.LocalUnits = 1
		}, want: "invalid provider-response evidence"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := textAuthorityManifest(t, policy, "json")
			for index := range manifest.Results {
				if manifest.Results[index].FormatID == "json" {
					test.mutate(&manifest.Results[index])
				}
			}
			require.ErrorContains(t, manifest.ValidateComplete(), test.want)
		})
	}
}

func TestTextManifestRequiresProviderResponseEvidence(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)
	_, err := policy.Authorize(manifest, "json")
	require.ErrorContains(t, err, "no enforceable unit bound")

	for index := range manifest.Results {
		if manifest.Results[index].FormatID == "json" {
			manifest.Results[index].ReasonCode = ""
			manifest.Results[index].UnitBoundMethod = UnitBoundProviderResponse
		}
	}
	require.NoError(t, manifest.ValidateComplete())
	_, err = policy.Authorize(manifest, "json")
	require.NoError(t, err)

	stale := manifest
	stale.Results = append([]CapabilityResult(nil), manifest.Results...)
	for index := range stale.Results {
		if stale.Results[index].FormatID == "json" {
			stale.Results[index].UnitBoundMethod = UnitBoundNone
			stale.Results[index].ReasonCode = reasonBoundUnitsMismatch
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
