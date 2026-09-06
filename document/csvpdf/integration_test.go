package csvpdf

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
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

func TestConvertedSourceUsesExistingPDFProcessing(t *testing.T) {
	source, _ := sourceFor(t, "name,value\ncafé,κόσμος Привет\n")
	converted, err := Convert(t.Context(), source, policyFor(t, DefaultLimits()))
	require.NoError(t, err)
	normalize, err := document.NewNormalizePolicy(100_000)
	require.NoError(t, err)
	policy, err := mistral.NewPolicy(mistral.PolicyConfig{Region: mistral.RegionEU, Model: mistral.DefaultModel, Retention: mistral.RetentionZDR, Training: mistral.TrainingOptedOut, MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 3, NormalizePolicy: normalize})
	require.NoError(t, err)
	manifest, err := mistraltest.SyntheticManifest(policy, true)
	require.NoError(t, err)
	authorization, err := policy.Authorize(manifest, "pdf")
	require.NoError(t, err)
	directory := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, safefileio.EnsurePrivateDir(directory))
	generated, err := converted.Source()
	require.NoError(t, err)
	prepared, err := mistral.Prepare(t.Context(), generated.Content, policy, mistral.PrepareOptions{Directory: directory, DeclaredMediaType: generated.MediaType, ExpectedSize: generated.Size, ExpectedSHA256: generated.SHA256, MaxSpoolBytes: 1 << 20, MinFreeBytes: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Release()) })
	require.Equal(t, converted.Receipt().PDFSHA256, prepared.SHA256())
	requests := 0
	client, err := mistral.NewClient(policy, mistral.ClientConfig{APIKey: "synthetic-key", HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
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
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"mistral-ocr-4-0","pages":[{"index":0,"markdown":"café κόσμος Привет"}],"usage_info":{"pages_processed":1}}`))}, nil
	})}})
	require.NoError(t, err)
	result, err := client.Process(t.Context(), prepared, authorization)
	require.NoError(t, err)
	require.Equal(t, 1, requests)
	require.Equal(t, 1, result.UnitsProcessed)
	require.NotZero(t, result.Document)
}
