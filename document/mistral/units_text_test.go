package mistral

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTextProviderResponseAuthorization(t *testing.T) {
	policy := testPolicy(t, 1<<20, MaxUnits)
	formatCases := []struct {
		format  string
		content []byte
	}{
		{format: "txt", content: []byte(strings.Repeat("line\n", MaxUnits+1))},
		{format: "markdown", content: []byte("# One\n\nbody\n")},
		{format: "csv", content: []byte(strings.Repeat("alpha,1\n", MaxUnits+1))},
		{format: "json", content: []byte(`{"value":"synthetic"}`)},
		{format: "jsonl", content: []byte(strings.Repeat("{\"value\":1}\n", MaxUnits+1))},
		{format: "yaml", content: []byte(strings.Repeat("---\nvalue: synthetic\n", MaxUnits+1))},
		{format: "go", content: []byte("package synthetic\n\nconst value = 1\n")},
		{format: "python", content: []byte("value = \"synthetic\"\n")},
		{format: "javascript", content: []byte("const value = \"synthetic\";\n")},
		{format: "rst", content: []byte("Synthetic\n========\n\nbody\n")},
		{format: "latex", content: []byte(`\documentclass{article}\begin{document}synthetic\end{document}`)},
		{format: "xml", content: append([]byte{0xef, 0xbb, 0xbf}, []byte("<synthetic/>\n")...)},
		{format: "eml", content: []byte("From: sender@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\nSubject: Synthetic\r\n\r\nbody\r\n")},
		{format: "msg", content: compoundDocument(t, "__properties_version1.0")},
	}
	manifest := textAuthorityManifest(t, policy, textFormatIDs()...)
	for _, test := range formatCases {
		t.Run(test.format, func(t *testing.T) {
			candidate, found := CandidateFormatByID(test.format)
			require.True(t, found)
			authorization, err := policy.Authorize(manifest, test.format)
			require.NoError(t, err)
			prepared := prepareTextDocument(t, policy, candidate, test.content)
			var requests atomic.Int64
			var uploaded []byte
			client, err := NewClient(policy, ClientConfig{
				APIKey: "synthetic-key", MaxRetries: 1,
				HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					requests.Add(1)
					body, readErr := io.ReadAll(request.Body)
					if readErr != nil {
						return nil, readErr
					}
					var envelope struct {
						Document struct {
							URL string `json:"document_url"`
						} `json:"document"`
					}
					if unmarshalErr := json.Unmarshal(body, &envelope); unmarshalErr != nil {
						return nil, unmarshalErr
					}
					encoded := strings.TrimPrefix(envelope.Document.URL, "data:"+candidate.MediaType+";base64,")
					uploaded, err = base64.StdEncoding.DecodeString(encoded)
					if err != nil {
						return nil, fmt.Errorf("decode synthetic upload: %w", err)
					}
					return &http.Response{
						StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(strings.NewReader(testSuccessfulResponse)), Request: request,
					}, nil
				})},
			})
			require.NoError(t, err)
			result, err := client.Process(t.Context(), prepared, authorization)
			require.NoError(t, err)
			assert.Equal(t, 1, result.UnitsProcessed)
			assert.Equal(t, int64(1), requests.Load())
			assert.Equal(t, test.content, uploaded)
		})
	}
}

func TestTextProviderResponseRejectsProviderOverflow(t *testing.T) {
	policy := testPolicy(t, 1<<20, MaxUnits)
	snapshot := preparedSnapshot{size: 1}
	page := wirePage{indexPresent: true}
	t.Run("pages", func(t *testing.T) {
		pages := make([]wirePage, MaxUnits+1)
		for index := range pages {
			pages[index] = page
			pages[index].Index = index
		}
		err := validateWireResult(wireResult{Model: defaultModel, Pages: pages, UsageInfo: &wireUsage{
			PagesProcessed: MaxUnits + 1, pagesProcessedPresent: true,
		}}, defaultModel, snapshot, UnitBoundProviderResponse, policy.values.MaxUnits)
		require.ErrorIs(t, err, ErrCapabilityContract)
	})
	t.Run("usage", func(t *testing.T) {
		err := validateWireResult(wireResult{Model: defaultModel, Pages: []wirePage{page}, UsageInfo: &wireUsage{
			PagesProcessed: MaxUnits + 1, pagesProcessedPresent: true,
		}}, defaultModel, snapshot, UnitBoundProviderResponse, policy.values.MaxUnits)
		require.ErrorIs(t, err, ErrCapabilityContract)
	})
}

func textFormatIDs() []string {
	return []string{
		"txt", "markdown", "csv", "json", "jsonl", "yaml", "go",
		"python", "javascript", "rst", "latex", "xml", "eml", "msg",
	}
}

func prepareTextDocument(t *testing.T, policy Policy, candidate CandidateFormat, content []byte) *PreparedDocument {
	t.Helper()
	digest := sha256.Sum256(content)
	directory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, directory)
	prepared, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(content)), policy, PrepareOptions{
		Directory: directory, DeclaredMediaType: candidate.MediaType,
		ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		MaxSpoolBytes: policy.values.MaxDocumentBytes, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Release()) })
	return prepared
}

func textAuthorityManifest(t *testing.T, policy Policy, formatIDs ...string) CapabilityManifest {
	t.Helper()
	manifest := syntheticManifest(t, policy, true)
	for index := range manifest.Results {
		if slices.Contains(formatIDs, manifest.Results[index].FormatID) {
			manifest.Results[index].ReasonCode = ""
			manifest.Results[index].UnitBoundMethod = UnitBoundProviderResponse
		}
	}
	require.NoError(t, manifest.ValidateComplete())
	return manifest
}
