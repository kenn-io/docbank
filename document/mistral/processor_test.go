package mistral

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

func TestProcessorSnapshotsCapabilityManifest(t *testing.T) {
	content := testPDF("processor snapshot")
	policy := testPolicy(t, 1024, 10)
	manifest := syntheticManifest(t, policy, true)
	providerBytes := int64(len(content))
	for index := range manifest.Results {
		if manifest.Results[index].FormatID == formatIDPDF {
			manifest.Results[index].ProviderBytes = &providerBytes
		}
	}
	client, err := NewClient(policy, ClientConfig{
		APIKey: "synthetic-key",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			_, _ = io.Copy(io.Discard, request.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"model":"mistral-ocr-4-0","pages":[{"index":0,"markdown":"# Synthetic snapshot"}],"usage_info":{"pages_processed":1}}`,
				)),
			}, nil
		})},
	})
	require.NoError(t, err)
	spoolDirectory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, spoolDirectory)
	processor, err := NewProcessor(ProcessorConfig{
		Client: client, Policy: policy, CapabilityManifest: manifest,
		SpoolDirectory: spoolDirectory, MaxSpoolBytes: 1024, MinFreeBytes: 1,
	})
	require.NoError(t, err)

	providerBytes = -1
	for index := range manifest.Results {
		if manifest.Results[index].FormatID == formatIDPDF {
			manifest.Results[index].Status = ProbeStatusRejected
		}
	}
	digest := sha256.Sum256(content)
	source, err := ocr.NewSource(
		io.NopCloser(bytes.NewReader(content)), mediaTypePDF, int64(len(content)), hex.EncodeToString(digest[:]),
	)
	require.NoError(t, err)

	result, err := processor.Process(t.Context(), source)
	require.NoError(t, err)
	require.NotEmpty(t, result.Document.Chunks)
	assert.Equal(t, []string{"Synthetic snapshot"}, result.Document.Chunks[0].HeadingPath)
	assert.Equal(t, processor.policyFingerprint, result.PolicyFingerprint)
}

func TestProcessorClassifiesStagingAndSourceFailures(t *testing.T) {
	content := testPDF("processor classification")
	digest := sha256.Sum256(content)
	newSource := func(hash string) ocr.Source {
		source, err := ocr.NewSource(
			io.NopCloser(bytes.NewReader(content)), mediaTypePDF, int64(len(content)), hash,
		)
		require.NoError(t, err)
		return source
	}

	missingDirectory := filepath.Join(t.TempDir(), "missing")
	processor := newProcessorWithoutRequests(t, missingDirectory)
	_, err := processor.Process(t.Context(), newSource(hex.EncodeToString(digest[:])))
	require.Error(t, err)
	assert.Equal(t, ocr.ErrorTransient, ocr.ErrorKindOf(err))
	assert.True(t, ocr.IsRetryable(err))

	spoolDirectory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, spoolDirectory)
	processor = newProcessorWithoutRequests(t, spoolDirectory)
	_, err = processor.Process(t.Context(), newSource(zeroSHA256()))
	require.Error(t, err)
	assert.Equal(t, ocr.ErrorInvalidInput, ocr.ErrorKindOf(err))
	assert.False(t, ocr.IsRetryable(err))
}

func TestClassifyProcessorErrorUsesCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := classifyProcessorError(ctx, errors.Join(errors.New("transport failed"), context.DeadlineExceeded), RequestMetrics{Requests: 1})
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, ocr.ErrorKindOf(err))
	assert.False(t, ocr.IsRetryable(err))
	assert.Equal(t, 1, ocr.MetricsFromError(err).Requests)

	metrics := RequestMetrics{Requests: 2, Retries: 1}
	err = classifyProcessorError(t.Context(), errors.Join(ErrTransientResponse, context.DeadlineExceeded), metrics)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, ocr.ErrorTransient, ocr.ErrorKindOf(err))
	assert.Equal(t, toOCRMetrics(metrics), ocr.MetricsFromError(err))
	assert.True(t, ocr.IsRetryable(err))
}

func newProcessorWithoutRequests(t *testing.T, spoolDirectory string) *Processor {
	t.Helper()
	policy := testPolicy(t, 1024, 10)
	client, err := NewClient(policy, ClientConfig{
		APIKey: "synthetic-key",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("staging failure unexpectedly reached the provider")
			return nil, errors.New("unexpected provider request")
		})},
	})
	require.NoError(t, err)
	processor, err := NewProcessor(ProcessorConfig{
		Client: client, Policy: policy, CapabilityManifest: syntheticManifest(t, policy, true),
		SpoolDirectory: spoolDirectory, MaxSpoolBytes: 1024, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	return processor
}
