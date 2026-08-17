package mistral

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunCapabilityProbeProducesCompleteSanitizedAuthority(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	fixtureConfig := generatedProbeFixtureConfig(t)
	transport := &probeTransport{t: t, boundVerified: true}
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
	policy := testPolicy(t, 1<<20, 10)
	transport := &probeTransport{t: t, boundVerified: false}
	client, err := NewClient(policy, ClientConfig{APIKey: "synthetic-key", HTTPClient: &http.Client{Transport: transport}})
	require.NoError(t, err)
	manifest, err := RunCapabilityProbe(t.Context(), client, ProbeConfig{
		Fixtures:   generatedProbeFixtureConfig(t),
		ObservedAt: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	pdf := findManifestResult(t, manifest, "pdf")
	assert.Equal(t, ProbeStatusPassed, pdf.Status)
	assert.Equal(t, UnitBoundNone, pdf.UnitBoundMethod)
	assert.Equal(t, reasonBoundUnverified, pdf.ReasonCode)
	_, err = policy.Authorize(manifest, "pdf")
	require.ErrorContains(t, err, "no enforceable unit bound")
}

type probeTransport struct {
	t             *testing.T
	boundVerified bool
	calls         int
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
	}
	require.NoError(transport.t, json.Unmarshal(body, &input))
	assert.True(transport.t, input.ExtractHeader)
	assert.True(transport.t, input.ExtractFooter)

	var candidateIndex int
	units := 1
	switch call {
	case 0:
		candidateIndex = 0
		units = 2
		assert.Equal(transport.t, "0-9", input.Pages)
	case 1:
		candidateIndex = 0
		assert.Equal(transport.t, "0-0", input.Pages)
		if !transport.boundVerified {
			units = 2
		}
	default:
		candidateIndex = call - 1
		assert.Empty(transport.t, input.Pages)
	}
	require.Less(transport.t, candidateIndex, len(candidateFormats))
	candidate := candidateFormats[candidateIndex]
	sentinel, err := ProbeFixtureSentinel(candidate.ID)
	require.NoError(transport.t, err)
	pages := make([]map[string]any, units)
	for index := range pages {
		markdown := ""
		if index == 0 {
			markdown = sentinel
		}
		pages[index] = map[string]any{"index": index, "markdown": markdown}
	}
	encoded, err := json.Marshal(map[string]any{
		"model": defaultModel, "pages": pages,
		"usage_info": map[string]int{"pages_processed": units},
	})
	require.NoError(transport.t, err)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(encoded)),
		Request:    request,
	}, nil
}

func generatedProbeFixtureConfig(t *testing.T) ProbeFixtureConfig {
	t.Helper()
	fixtureDirectory := filepath.Join(t.TempDir(), "fixtures")
	require.NoError(t, WriteProbeFixtures(t.Context(), fixtureDirectory, FixtureOptions{
		SeedDirectory: writeNativeSeeds(t),
	}))
	spoolDirectory := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, os.Mkdir(spoolDirectory, 0o700))
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
