package mistral

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
