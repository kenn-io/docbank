package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/document/mistral/mistraltest"
)

func TestNewMistralProbeClientRequiresMistralRuntimeAndCredential(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	t.Setenv("DOCBANK_TEST_MISTRAL_OCR_KEY", "synthetic-key")
	_, client, output, err := newMistralProbeClient(t.Context(), cfg, "documents")
	require.NoError(t, err)
	require.NotNil(t, client)
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
		Transport: transport,
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
	require.ErrorContains(t, runMistralProbe(t.Context(), &printed, client, policy, seeds, output, false), "pass --overwrite")
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
