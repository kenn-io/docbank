package mistral

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mistral/internal/testfixture"
)

func TestRunCapabilityProbeProducesCompleteSanitizedAuthority(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	fixtureConfig := generatedProbeFixtureConfig(t)
	transport := &testfixture.ProbeTransport{T: t, Model: defaultModel, ExtractHeader: true, ExtractFooter: true}
	client, err := NewClient(policy, ClientConfig{APIKey: "synthetic-key", HTTPClient: &http.Client{Transport: transport}})
	require.NoError(t, err)

	manifest, err := RunCapabilityProbe(t.Context(), client, ProbeConfig{
		Fixtures:   fixtureConfig,
		ObservedAt: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	require.NoError(t, manifest.ValidateComplete())
	assert.Equal(t, len(candidateFormats)+1, transport.Calls)
	require.Len(t, manifest.Results, len(candidateFormats))
	pdf := findManifestResult(t, manifest, "pdf")
	assert.Equal(t, ProbeStatusPassed, pdf.Status)
	assert.Equal(t, UnitBoundProviderRequest, pdf.UnitBoundMethod)
	assert.Equal(t, 2, pdf.FixtureUnits)
	assert.Equal(t, 1, pdf.BoundRequestedUnits)
	assert.Equal(t, 1, pdf.BoundUnitsProcessed)
	docx := findManifestResult(t, manifest, "docx")
	assert.Equal(t, ProbeStatusPassed, docx.Status)
	assert.Equal(t, UnitBoundNone, docx.UnitBoundMethod)
	assert.Empty(t, docx.ReasonCode)
	pptx := findManifestResult(t, manifest, "pptx")
	assert.Equal(t, ProbeStatusPassed, pptx.Status)
	assert.Equal(t, UnitBoundLocalExact, pptx.UnitBoundMethod)
	assert.Equal(t, 1, pptx.LocalUnits)
	t.Logf("pptx status=%q unit_bound_method=%q local_units=%d", pptx.Status, pptx.UnitBoundMethod, pptx.LocalUnits)
	for _, formatID := range textFormatIDs() {
		result := findManifestResult(t, manifest, formatID)
		assert.Equal(t, ProbeStatusPassed, result.Status)
		assert.Equal(t, UnitBoundProviderResponse, result.UnitBoundMethod)
		assert.Zero(t, result.LocalUnits)
		_, err = policy.Authorize(manifest, formatID)
		require.NoError(t, err)
	}
	xlsx := findManifestResult(t, manifest, "xlsx")
	assert.Equal(t, ProbeStatusPassed, xlsx.Status)
	assert.Equal(t, UnitBoundNone, xlsx.UnitBoundMethod)
	assert.Zero(t, xlsx.LocalUnits)
	t.Logf("xlsx status=%q unit_bound_method=%q local_units=%d", xlsx.Status, xlsx.UnitBoundMethod, xlsx.LocalUnits)

	authorization, err := policy.Authorize(manifest, "pdf")
	require.NoError(t, err)
	assert.Equal(t, "pdf", authorization.Format().ID)
	assert.NotEmpty(t, authorization.PolicyFingerprint())
	_, err = policy.Authorize(manifest, "pptx")
	require.NoError(t, err)
	_, err = policy.Authorize(manifest, "xlsx")
	require.ErrorContains(t, err, "no enforceable unit bound")
	_, err = policy.Authorize(manifest, "docx")
	require.ErrorContains(t, err, "run the authenticated capability probe and supply its manifest")

	var encoded bytes.Buffer
	require.NoError(t, EncodeCapabilityManifest(&encoded, manifest))
	for _, candidate := range candidateFormats {
		sentinel, sentinelErr := ProbeFixtureSentinel(candidate.ID)
		require.NoError(t, sentinelErr)
		assert.NotContains(t, encoded.String(), sentinel)
	}
}

func TestDOCXRouteLeavesNativeProbeUnchanged(t *testing.T) {
	policy := testPolicyWithRenderPDF(t, testRenderPDFPolicy(t, testMultipagePDF(1), nil), 1<<20, 10)
	transport := &testfixture.ProbeTransport{T: t, Model: defaultModel, ExtractHeader: true, ExtractFooter: true}
	client, err := NewClient(policy, ClientConfig{APIKey: "synthetic-key", HTTPClient: &http.Client{Transport: transport}})
	require.NoError(t, err)
	manifest, err := RunCapabilityProbe(t.Context(), client, ProbeConfig{
		Fixtures:   generatedProbeFixtureConfig(t),
		ObservedAt: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	docx := findManifestResult(t, manifest, "docx")
	candidate, ok := CandidateFormatByID("docx")
	require.True(t, ok)
	assert.Equal(t, UnitBoundNone, docx.UnitBoundMethod)
	assert.Equal(t, requestFingerprint(candidate, probeRequestOptions(
		candidate, manifest.MaxUnits, policy.values.ExtractHeader, policy.values.ExtractFooter,
	)), docx.RequestFingerprint)
	authorization, err := policy.Authorize(manifest, "docx")
	require.NoError(t, err)
	assert.Equal(t, UnitBoundLocalExact, authorization.method)
}

func TestRunCapabilityProbeRecordsUnverifiedProviderBoundWithoutAuthority(t *testing.T) {
	tests := []struct {
		name       string
		boundMode  string
		wantReason string
	}{
		{name: "provider request fails", boundMode: "request_failed", wantReason: reasonBoundRequestFailed},
		{name: "provider ignores bound", boundMode: "units_mismatch", wantReason: reasonBoundUnitsMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := testPolicy(t, 1<<20, 10)
			transport := &testfixture.ProbeTransport{T: t, Model: defaultModel, ExtractHeader: true, ExtractFooter: true, BoundMode: test.boundMode}
			client, err := NewClient(policy, ClientConfig{
				APIKey: "synthetic-key", MaxRetryDelay: time.Millisecond,
				HTTPClient: &http.Client{Transport: transport},
			})
			require.NoError(t, err)
			manifest, err := RunCapabilityProbe(t.Context(), client, ProbeConfig{
				Fixtures:   generatedProbeFixtureConfig(t),
				ObservedAt: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
			})
			require.NoError(t, err)
			pdf := findManifestResult(t, manifest, "pdf")
			assert.Equal(t, ProbeStatusPassed, pdf.Status)
			assert.Equal(t, UnitBoundNone, pdf.UnitBoundMethod)
			assert.Equal(t, test.wantReason, pdf.ReasonCode)
			_, err = policy.Authorize(manifest, "pdf")
			require.ErrorContains(t, err, "no enforceable unit bound")
		})
	}
}

func TestProbeExplainsFixtureOutsideBoundRange(t *testing.T) {
	policy := testPolicy(t, 1<<20, 3)
	client := &Client{policy: policy}
	pdf, found := CandidateFormatByID("pdf")
	require.True(t, found)
	result := CapabilityResult{UnitCount: 3, UnitBoundMethod: UnitBoundNone}

	observeUnitBound(t.Context(), client, probeFixture{}, pdf, &result)
	assert.Equal(t, UnitBoundNone, result.UnitBoundMethod)
	assert.Equal(t, reasonBoundFixtureOutOfRange, result.ReasonCode)
}

func TestRunCapabilityProbeClassifiesSanitizedFailures(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		wantStatus ProbeStatus
		wantReason string
	}{
		{name: "provider rejection", mode: "provider_4xx", wantStatus: ProbeStatusRejected, wantReason: "provider_4xx"},
		{name: "invalid model", mode: "invalid_model", wantStatus: ProbeStatusRejected, wantReason: "invalid_model"},
		{name: "transient exhaustion", mode: "transient_exhausted", wantStatus: ProbeStatusFailed, wantReason: "transient_exhausted"},
		{name: "empty output", mode: "empty_output", wantStatus: ProbeStatusFailed, wantReason: "empty_output"},
		{name: "missing sentinel", mode: "sentinel_missing", wantStatus: ProbeStatusFailed, wantReason: "sentinel_missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := testPolicy(t, 1<<20, 10)
			transport := &testfixture.ProbeTransport{T: t, Model: defaultModel, ExtractHeader: true, ExtractFooter: true, FailureMode: test.mode}
			client, err := NewClient(policy, ClientConfig{
				APIKey: "synthetic-key", MaxRetryDelay: time.Millisecond,
				HTTPClient: &http.Client{Transport: transport},
			})
			require.NoError(t, err)
			manifest, err := RunCapabilityProbe(t.Context(), client, ProbeConfig{
				Fixtures: generatedProbeFixtureConfig(t), ObservedAt: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
			})
			require.NoError(t, err)
			pdf := findManifestResult(t, manifest, "pdf")
			assert.Equal(t, test.wantStatus, pdf.Status)
			assert.Equal(t, test.wantReason, pdf.ReasonCode)
		})
	}
}

func TestRunCapabilityProbePropagatesCancellation(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	transport := &testfixture.ProbeTransport{T: t, Model: defaultModel, ExtractHeader: true, ExtractFooter: true}
	client, err := NewClient(policy, ClientConfig{
		APIKey: "synthetic-key", HTTPClient: &http.Client{Transport: transport},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = RunCapabilityProbe(ctx, client, ProbeConfig{Fixtures: generatedProbeFixtureConfig(t)})
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, transport.Calls)
}

func probeHTTPResponse(request *http.Request, status int, body string) *http.Response {
	return testfixture.ProbeHTTPResponse(request, status, body)
}

func generatedProbeFixtureConfig(t *testing.T) ProbeFixtureConfig {
	t.Helper()
	fixtureDirectory := newProbeFixtureDestination(t, "fixtures")
	require.NoError(t, WriteProbeFixtures(t.Context(), fixtureDirectory, FixtureOptions{
		SeedDirectory: writeNativeSeeds(t),
	}))
	spoolDirectory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, spoolDirectory)
	return ProbeFixtureConfig{
		FixtureDirectory: fixtureDirectory, SpoolDirectory: spoolDirectory,
		MaxSpoolBytes: 32 << 20, MinFreeBytes: 1,
	}
}

func findManifestResult(t *testing.T, manifest CapabilityManifest, formatID string) CapabilityResult {
	t.Helper()
	for _, result := range manifest.Results {
		if result.FormatID == formatID {
			return result
		}
	}
	require.FailNow(t, fmt.Sprintf("manifest result %q not found", formatID))
	return CapabilityResult{}
}
