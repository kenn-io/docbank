package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	policy, err := cfg.RenditionProfiles["documents"].MistralPolicy()
	require.NoError(t, err)
	requests := 0
	client, err := mistral.NewClient(policy, mistral.ClientConfig{APIKey: "synthetic-key", HTTPClient: &http.Client{
		Transport: mistralProbeTransport(func(request *http.Request) (*http.Response, error) {
			requests++
			assert.Equal(t, "https://api.eu.mistral.ai/v1/ocr", request.URL.String())
			assert.Equal(t, "Bearer synthetic-key", request.Header.Get("Authorization"))
			return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"type":"invalid_model"}`)), Request: request}, nil
		}),
	}})
	require.NoError(t, err)
	seeds := t.TempDir()
	require.NoError(t, mistraltest.WriteNativeSeeds(seeds))
	output := filepath.Join(t.TempDir(), "manifest.json")
	var printed bytes.Buffer
	require.NoError(t, runMistralProbe(t.Context(), &printed, client, policy, seeds, output, false))
	content, err := os.ReadFile(output)
	require.NoError(t, err)
	manifest, err := mistral.DecodeCapabilityManifest(bytes.NewReader(content))
	require.NoError(t, err)
	assert.Equal(t, mistral.DefaultModel, manifest.RequestedModel)
	assert.Equal(t, len(mistral.CandidateFormats()), requests)
	for _, result := range manifest.Results {
		assert.Equal(t, "invalid_model", result.ReasonCode)
		assert.Contains(t, printed.String(), result.FormatID+" "+string(result.Status)+" invalid_model")
	}
	assert.Contains(t, printed.String(), output)
	require.Error(t, runMistralProbe(t.Context(), &printed, client, policy, seeds, output, false))
	preserved, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Equal(t, content, preserved)
	require.NoError(t, runMistralProbe(t.Context(), &printed, client, policy, seeds, output, true))
}

type mistralProbeTransport func(*http.Request) (*http.Response, error)

func (transport mistralProbeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
