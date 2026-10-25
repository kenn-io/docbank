package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
	_, client, configured, err := newMistralProbeClient(t.Context(), cfg, "documents")
	require.NoError(t, err)
	require.NotNil(t, client)
	assert.Equal(t, cfg.RenditionProfiles["documents"].Runtime.CapabilityManifest, configured.Runtime.CapabilityManifest)

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
	configured.DescriptorFingerprint = strings.Repeat("0", 64)
	var printed bytes.Buffer
	require.NoError(t, runMistralProbe(t.Context(), &printed, client, policy, configured, seeds, false))
	content, err := os.ReadFile(output)
	require.NoError(t, err)
	manifest, err := mistral.DecodeCapabilityManifest(bytes.NewReader(content))
	require.NoError(t, err)
	for _, result := range manifest.Results {
		assert.Contains(t, printed.String(), result.FormatID+" "+string(result.Status)+" "+result.ReasonCode)
	}
	assert.Contains(t, printed.String(), output)
	assert.Contains(t, printed.String(), "Set these fingerprints in the rendition profile, then restart the daemon.")

	// The printed fingerprints are the ones daemon startup accepts for the saved manifest.
	fingerprints := regexp.MustCompile(`(?m)^(descriptor|disclosure)_fingerprint = "([0-9a-f]{64})"$`).
		FindAllStringSubmatch(printed.String(), -1)
	require.Len(t, fingerprints, 2)
	configured.DescriptorFingerprint, configured.DisclosureFingerprint = fingerprints[0][2], fingerprints[1][2]
	cfg.RenditionProfiles["documents"] = configured
	providers, _, err := configureRenditionProviders(cfg)
	require.NoError(t, err)
	require.Contains(t, providers, "documents")

	require.ErrorContains(t, runMistralProbe(t.Context(), &printed, client, policy, configured, seeds, false), "pass --overwrite")
	preserved, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Equal(t, content, preserved)
	printed.Reset()
	require.NoError(t, runMistralProbe(t.Context(), &printed, client, policy, configured, seeds, true))
	assert.Contains(t, printed.String(), "These fingerprints already match the rendition profile.")

	content, err = os.ReadFile(output)
	require.NoError(t, err)
	failedClient, err := mistral.NewClient(policy, mistral.ClientConfig{APIKey: "synthetic-key", MaxRetryDelay: time.Millisecond, HTTPClient: &http.Client{
		Transport: mistralProbeTransport(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"type":"invalid_model"}`)), Request: request}, nil
		}),
	}})
	require.NoError(t, err)
	printed.Reset()
	require.Error(t, runMistralProbe(t.Context(), &printed, failedClient, policy, configured, seeds, true))
	preserved, err = os.ReadFile(output)
	require.NoError(t, err)
	assert.Equal(t, content, preserved)
	for _, candidate := range mistral.CandidateFormats() {
		assert.Contains(t, printed.String(), candidate.ID+" provider_rejected invalid_model")
	}
	assert.NotContains(t, printed.String(), "Capability manifest saved")
}

func TestRunMistralProbeRemovesTemporaryFilesWhenCanceled(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	configured := cfg.RenditionProfiles["documents"]
	policy, err := configured.MistralPolicy()
	require.NoError(t, err)
	temporary := t.TempDir()
	// os.TempDir reads TMPDIR on Unix and TMP or TEMP on Windows.
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, temporary)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client, err := mistral.NewClient(policy, mistral.ClientConfig{APIKey: "synthetic-key", HTTPClient: &http.Client{
		Transport: mistralProbeTransport(func(request *http.Request) (*http.Response, error) {
			cancel()
			return nil, request.Context().Err()
		}),
	}})
	require.NoError(t, err)
	seeds := t.TempDir()
	require.NoError(t, mistraltest.WriteNativeSeeds(seeds))
	configured.Runtime.CapabilityManifest = filepath.Join(t.TempDir(), "manifest.json")

	err = runMistralProbe(ctx, io.Discard, client, policy, configured, seeds, false)
	require.ErrorIs(t, err, context.Canceled)
	entries, err := os.ReadDir(temporary)
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.NoFileExists(t, configured.Runtime.CapabilityManifest)
}

type mistralProbeTransport func(*http.Request) (*http.Response, error)

func (transport mistralProbeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
