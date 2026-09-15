package docxpdf

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/document/mistral/mistraltest"
	"go.kenn.io/kit/safefileio"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConvertedDOCXUsesExistingPDFProcessing(t *testing.T) {
	t.Run("within MaxUnits", func(t *testing.T) { testConvertedDOCXProcessing(t, "ok", false) })
	t.Run("above MaxUnits", func(t *testing.T) { testConvertedDOCXProcessing(t, "pages", true) })
}

func testConvertedDOCXProcessing(t *testing.T, mode string, wantReject bool) {
	t.Helper()
	normalize, err := document.NewNormalizePolicy(100_000)
	require.NoError(t, err)
	policy, err := mistral.NewPolicy(mistral.PolicyConfig{
		Region: mistral.RegionEU, Model: mistral.DefaultModel, Retention: mistral.RetentionZDR,
		Training: mistral.TrainingOptedOut, MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20,
		MaxUnits: 3, NormalizePolicy: normalize,
	})
	require.NoError(t, err)
	manifest, err := mistraltest.SyntheticManifest(policy, true)
	require.NoError(t, err)
	authorization, err := policy.Authorize(manifest, "pdf")
	require.NoError(t, err)
	var converted *Result
	requests := 0
	client, err := mistral.NewClient(policy, mistral.ClientConfig{
		APIKey: "synthetic-key",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			body, readErr := io.ReadAll(request.Body)
			require.NoError(t, readErr)
			var payload struct {
				Model    string `json:"model"`
				Pages    string `json:"pages"`
				Document struct {
					Type string `json:"type"`
					URL  string `json:"document_url"`
				} `json:"document"`
			}
			require.NoError(t, json.Unmarshal(body, &payload))
			require.Equal(t, mistral.DefaultModel, payload.Model)
			require.Equal(t, "0-2", payload.Pages)
			require.Equal(t, "document_url", payload.Document.Type)
			require.True(t, strings.HasPrefix(payload.Document.URL, "data:application/pdf;base64,"))
			pdf, decodeErr := base64.StdEncoding.DecodeString(strings.TrimPrefix(payload.Document.URL, "data:application/pdf;base64,"))
			require.NoError(t, decodeErr)
			require.True(t, bytes.Equal(converted.PDF(), pdf))
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"model":"mistral-ocr-4-0","pages":[{"index":0,"markdown":"page one"},{"index":1,"markdown":"page two"},{"index":2,"markdown":"page three"}],"usage_info":{"pages_processed":3}}`)),
			}, nil
		})},
	})
	require.NoError(t, err)
	limits := DefaultLimits()
	limits.MaxPages = policy.Values().MaxUnits
	source, _ := sourceFor(t, syntheticDOCX())
	converted, err = Convert(t.Context(), source, policyFor(t, helperExecutable(t, mode), limits))
	if wantReject {
		require.Nil(t, converted)
		require.EqualError(t, err, "generated PDF exceeds page limit or has no pages")
		require.Zero(t, requests)
		t.Log("MaxUnits=3 rendered_pages=4 result_nil=true provider_requests=0")
		return
	}
	require.NoError(t, err)
	directory := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, safefileio.EnsurePrivateDir(directory))
	generated, err := converted.Source()
	require.NoError(t, err)
	prepared, err := mistral.Prepare(t.Context(), generated.Content, policy, mistral.PrepareOptions{
		Directory: directory, DeclaredMediaType: generated.MediaType, ExpectedSize: generated.Size,
		ExpectedSHA256: generated.SHA256, MaxSpoolBytes: 1 << 20, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Release()) })
	require.Equal(t, converted.Receipt().PDFSHA256, prepared.SHA256())
	result, err := client.Process(t.Context(), prepared, authorization)
	require.NoError(t, err)
	require.Equal(t, 1, requests)
	require.Equal(t, converted.Receipt().Pages, result.UnitsProcessed)
	require.NotZero(t, result.Document)
}
