package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/document/mistral/mistraltest"
)

func TestNewMistralProbeClient(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	t.Setenv("DOCBANK_TEST_MISTRAL_OCR_KEY", "synthetic-key")
	policy, client, output, err := newMistralProbeClient(t.Context(), cfg, "documents")
	require.NoError(t, err)
	require.NotNil(t, client)
	assert.Equal(t, mistral.DefaultModel, policy.Values().Model)
	assert.Equal(t, cfg.RenditionProfiles["documents"].Runtime.CapabilityManifest, output)

	t.Setenv("DOCBANK_TEST_MISTRAL_OCR_KEY", "")
	_, client, _, err = newMistralProbeClient(t.Context(), cfg, "documents")
	require.ErrorContains(t, err, "credential environment variable is unavailable")
	require.Nil(t, client)
	cfg, _ = doclingDocumentProcessingConfig(t, "https://example.test")
	_, client, _, err = newMistralProbeClient(t.Context(), cfg, "documents")
	require.ErrorContains(t, err, "Mistral OCR")
	require.Nil(t, client)
}

func TestRunMistralProbePublishesManifestWithoutReplacingExistingFile(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	configured := cfg.RenditionProfiles["documents"]
	policy, err := configured.MistralPolicy()
	require.NoError(t, err)
	transport := mistraltest.NewProbeTransport(t)
	client, err := mistral.NewClient(policy, mistral.ClientConfig{APIKey: "synthetic-key", HTTPClient: &http.Client{
		Transport: mistralProbeTransport(func(request *http.Request) (*http.Response, error) {
			assert.Equal(t, "https://api.eu.mistral.ai/v1/ocr", request.URL.String())
			assert.Equal(t, "Bearer synthetic-key", request.Header.Get("Authorization"))
			return transport.RoundTrip(request)
		}),
	}})
	require.NoError(t, err)
	seeds := t.TempDir()
	require.NoError(t, mistraltest.WriteNativeSeeds(seeds))
	output := filepath.Join(t.TempDir(), "manifest.json")
	configured.Runtime.CapabilityManifest = output
	var printed bytes.Buffer
	require.NoError(t, runMistralProbe(t.Context(), &printed, client, policy, seeds, output, false))
	profile, err := configuredMistralProfile(configured)
	require.NoError(t, err)
	_, err = mistral.NewRenditionProvider(profile, environmentCredentialSecrets{}, &http.Client{})
	require.NoError(t, err)
	content, err := os.ReadFile(output)
	require.NoError(t, err)
	manifest, err := mistral.DecodeCapabilityManifest(bytes.NewReader(content))
	require.NoError(t, err)
	for _, result := range manifest.Results {
		assert.Contains(t, printed.String(), result.FormatID+" "+string(result.Status)+" "+result.ReasonCode)
	}
	assert.Contains(t, printed.String(), output)
	require.ErrorContains(t, runMistralProbe(t.Context(), &printed, client, policy, seeds, output, false), "download destination "+strconv.Quote(output)+" already exists; pass --overwrite")
	preserved, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Equal(t, content, preserved)
	require.NoError(t, runMistralProbe(t.Context(), &printed, client, policy, seeds, output, true))

	content, err = os.ReadFile(output)
	require.NoError(t, err)
	for _, test := range []struct {
		name         string
		status       int
		body, result string
	}{
		{"invalid model", http.StatusBadRequest, `{"type":"invalid_model"}`, "provider_rejected invalid_model"},
		{"rate limited", http.StatusTooManyRequests, `{}`, "probe_failed transient_exhausted"},
		{"bad key", http.StatusUnauthorized, `{}`, "provider_rejected provider_4xx"},
	} {
		t.Run(test.name, func(t *testing.T) {
			failedClient, err := mistral.NewClient(policy, mistral.ClientConfig{APIKey: "synthetic-key", MaxRetryDelay: time.Millisecond, HTTPClient: &http.Client{
				Transport: mistralProbeTransport(func(request *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: test.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(test.body)), Request: request}, nil
				}),
			}})
			require.NoError(t, err)
			var printed bytes.Buffer
			require.Error(t, runMistralProbe(t.Context(), &printed, failedClient, policy, seeds, output, true))
			preserved, err := os.ReadFile(output)
			require.NoError(t, err)
			assert.Equal(t, content, preserved)
			for _, candidate := range mistral.CandidateFormats() {
				assert.Contains(t, printed.String(), candidate.ID+" "+test.result)
			}
			assert.NotContains(t, printed.String(), "Capability manifest saved")
		})
	}
}

type mistralProbeTransport func(*http.Request) (*http.Response, error)

func (transport mistralProbeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
