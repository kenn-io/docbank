package mistral

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunCapabilityProbeProducesCompleteSanitizedAuthority(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	fixtureConfig := generatedProbeFixtureConfig(t)
	transport := &probeTransport{t: t}
	client, err := NewClient(policy, ClientConfig{APIKey: "synthetic-key", HTTPClient: &http.Client{Transport: transport}})
	require.NoError(t, err)

	manifest, err := RunCapabilityProbe(t.Context(), client, ProbeConfig{
		Fixtures:   fixtureConfig,
		ObservedAt: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	require.NoError(t, manifest.ValidateComplete())
	assert.Equal(t, len(candidateFormats)+1, transport.calls)
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

	authorization, err := policy.Authorize(manifest, "pdf")
	require.NoError(t, err)
	assert.Equal(t, "pdf", authorization.Format().ID)
	assert.NotEmpty(t, authorization.PolicyFingerprint())
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
			transport := &probeTransport{t: t, boundMode: test.boundMode}
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
		{name: "transient exhaustion", mode: "transient_exhausted", wantStatus: ProbeStatusFailed, wantReason: "transient_exhausted"},
		{name: "empty output", mode: "empty_output", wantStatus: ProbeStatusFailed, wantReason: "empty_output"},
		{name: "missing sentinel", mode: "sentinel_missing", wantStatus: ProbeStatusFailed, wantReason: "sentinel_missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := testPolicy(t, 1<<20, 10)
			transport := &probeTransport{t: t, failureMode: test.mode}
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
	transport := &probeTransport{t: t}
	client, err := NewClient(policy, ClientConfig{
		APIKey: "synthetic-key", HTTPClient: &http.Client{Transport: transport},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = RunCapabilityProbe(ctx, client, ProbeConfig{Fixtures: generatedProbeFixtureConfig(t)})
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, transport.calls)
}

type probeTransport struct {
	t           *testing.T
	boundMode   string
	failureMode string
	calls       int
}

func (transport *probeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.t.Helper()
	call := transport.calls
	transport.calls++
	require.Equal(transport.t, "Bearer synthetic-key", request.Header.Get("Authorization"), "call %d", call)
	body, err := io.ReadAll(request.Body)
	require.NoError(transport.t, err)
	var input struct {
		Pages         string `json:"pages"`
		ExtractHeader bool   `json:"extract_header"`
		ExtractFooter bool   `json:"extract_footer"`
		Document      struct {
			URL string `json:"document_url"`
		} `json:"document"`
	}
	require.NoError(transport.t, json.Unmarshal(body, &input))
	assert.True(transport.t, input.ExtractHeader)
	assert.True(transport.t, input.ExtractFooter)

	mediaType := strings.TrimPrefix(strings.SplitN(input.Document.URL, ";base64,", 2)[0], "data:")
	candidate, found := candidateByMediaType(mediaType)
	require.True(transport.t, found)
	if candidate.ID == "pdf" && input.Pages == "0-0" && transport.boundMode == "request_failed" {
		return probeHTTPResponse(request, http.StatusServiceUnavailable, `{}`), nil
	}
	if candidate.ID == "pdf" {
		switch transport.failureMode {
		case "provider_4xx":
			return probeHTTPResponse(request, http.StatusBadRequest, `{}`), nil
		case "transient_exhausted":
			return probeHTTPResponse(request, http.StatusServiceUnavailable, `{}`), nil
		}
	}

	units := 1
	if candidate.ID == "pdf" {
		switch input.Pages {
		case "0-9":
			units = 2
			assert.Equal(transport.t, "0-9", input.Pages)
		case "0-0":
			assert.Equal(transport.t, "0-0", input.Pages)
			if transport.boundMode == "units_mismatch" {
				units = 2
			}
		}
	} else {
		assert.Empty(transport.t, input.Pages)
	}
	sentinel, err := ProbeFixtureSentinel(candidate.ID)
	require.NoError(transport.t, err)
	pages := make([]map[string]any, units)
	for index := range pages {
		markdown := ""
		if index == 0 {
			switch transport.failureMode {
			case "empty_output":
			case "sentinel_missing":
				markdown = "synthetic text without the sentinel"
			default:
				markdown = sentinel
			}
		}
		pages[index] = map[string]any{"index": index, "markdown": markdown}
	}
	encoded, err := json.Marshal(map[string]any{
		"model": defaultModel, "pages": pages,
		"usage_info": map[string]int{"pages_processed": units},
	})
	require.NoError(transport.t, err)
	return probeHTTPResponse(request, http.StatusOK, string(encoded)), nil
}

func probeHTTPResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": []string{mediaTypeJSON}},
		Body: io.NopCloser(strings.NewReader(body)), Request: request,
	}
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
