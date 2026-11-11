package vision

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/vision/visiontest"
)

func TestUnderstandingRendersSections(t *testing.T) {
	raw := `{"visible_text":"FINE","literal_description":"A dog sits beside flames.","possible_meaning":"May joke about ignoring a problem.","uncertainty":"No conversation context is available."}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body request
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			return
		}
		assert.Contains(t, body.Messages[0].Content[0].Text, "Do not infer")
		assert.Equal(t, "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("synthetic-image")), body.Messages[0].Content[1].ImageURL.URL)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(visiontest.Completion(raw))
	}))
	defer server.Close()
	p, err := New(Profile{DescriptorID: UnderstandingProviderID, Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
	require.NoError(t, err)
	data := []byte("synthetic-image")
	meta := understandingMetadata(data)
	result, err := document.RenderRendition(t.Context(), p, &testUpload{ReadCloser: io.NopCloser(bytes.NewReader(data)), metadata: meta}, understandingAuthorization(p, meta))
	require.NoError(t, err)
	assert.Empty(t, result.ProviderMarkdown)
	require.Len(t, result.Artifacts, 1)
	assert.Equal(t, document.EvidenceArtifactStructured, result.Artifacts[0].Role)
	assert.JSONEq(t, raw, string(result.Artifacts[0].Payload))
	assert.Len(t, result.Evidence.Omissions, 1)
	assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
	require.Len(t, result.Evidence.Units, 4)
	for i, section := range []struct{ heading, text string }{
		{"Visible text (best-effort transcription)", "FINE"},
		{"Literal description", "A dog sits beside flames."},
		{"Possible meaning (interpretation)", "May joke about ignoring a problem."},
		{"Uncertainty and missing context", "No conversation context is available."},
	} {
		assert.Equal(t, []string{document.ImageUnderstandingHeading, section.heading}, result.Evidence.Units[i].HeadingPath)
		assert.Equal(t, section.text, result.Evidence.Units[i].Text)
	}
	assert.Equal(t, int64(4), result.Receipt.Usage.Units)
	assert.Equal(t, meta.SHA256, result.Receipt.SourceSHA256)
	assert.Equal(t, p.descriptor.Fingerprint, result.Receipt.DescriptorFingerprint)

	t.Run("all empty", func(t *testing.T) {
		empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(visiontest.Completion(`{"visible_text":"","literal_description":" ","possible_meaning":"","uncertainty":""}`))
		}))
		defer empty.Close()
		p, err := New(Profile{DescriptorID: UnderstandingProviderID, Origin: empty.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, empty.Client())
		require.NoError(t, err)
		_, err = document.RenderRendition(t.Context(), p, &testUpload{ReadCloser: io.NopCloser(bytes.NewReader(data)), metadata: meta}, understandingAuthorization(p, meta))
		classified, ok := errors.AsType[*document.RenditionProviderError](err)
		require.True(t, ok)
		assert.Equal(t, document.RenditionErrorMalformedEvidence, classified.Code())
	})
}

func TestUnderstandingParsesBoundedReplies(t *testing.T) {
	payload := `{"visible_text":"FINE","literal_description":"","possible_meaning":"","uncertainty":""}`
	for _, raw := range []string{"```json\n" + payload + "\n```", "Here is the result:\n" + payload} {
		value, err := parseUnderstanding([]byte(raw))
		require.NoError(t, err, raw)
		assert.Equal(t, "FINE", *value.VisibleText)
	}
	partial, err := parseUnderstanding([]byte(payload))
	require.NoError(t, err)
	assert.Len(t, partial.evidence("synthetic-vision").Omissions, 4)
	for _, raw := range []string{
		`{"literal_description":"","possible_meaning":"","uncertainty":""}`,
		`{"visible_text":"","possible_meaning":"","uncertainty":""}`,
		`{"visible_text":"","literal_description":"","uncertainty":""}`,
		`{"visible_text":"","literal_description":"","possible_meaning":""}`,
		`{"visible_text":null,"literal_description":"","possible_meaning":"","uncertainty":""}`,
		`{"visible_text":"","literal_description":"","possible_meaning":"","uncertainty":"\u0000"}`,
		`{"visible_text":"` + strings.Repeat("x", 16385) + `","literal_description":"","possible_meaning":"","uncertainty":""}`,
	} {
		_, err := parseUnderstanding([]byte(raw))
		assert.Error(t, err, "reply %q", raw[:min(len(raw), 100)])
	}
}

func TestUnderstandingRemovesControlsFromPublishedResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(visiontest.Completion(`{"visible_text":"  A\tB\u00a0C\r\nD\u001b[2J\u0007","literal_description":"\u001b\u0007","possible_meaning":"meaning","uncertainty":"unknown"}`))
	}))
	defer server.Close()
	p, err := New(Profile{DescriptorID: UnderstandingProviderID, Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
	require.NoError(t, err)
	data := []byte("synthetic-image")
	meta := understandingMetadata(data)
	result, err := document.RenderRendition(t.Context(), p, &testUpload{ReadCloser: io.NopCloser(bytes.NewReader(data)), metadata: meta}, understandingAuthorization(p, meta))
	require.NoError(t, err)
	require.Len(t, result.Evidence.Units, 3)
	assert.Equal(t, "  A\tB\u00a0C\nD[2J", result.Evidence.Units[0].Text)
	assert.Contains(t, result.Evidence.Omissions, document.SourceEvidenceOmissionV1{Kind: document.EvidenceOmissionField, Field: "literal_description", Reason: "no supported content provided"})
	require.Len(t, result.Artifacts, 1)
	assert.JSONEq(t, `{"visible_text":"  A\tB\u00a0C\nD[2J","literal_description":"","possible_meaning":"meaning","uncertainty":"unknown"}`, string(result.Artifacts[0].Payload))
}

func understandingMetadata(data []byte) document.AuthorizedUploadMetadata {
	return document.AuthorizedUploadMetadata{MediaFamily: "image", MediaType: "image/png", InputKind: document.RenditionInputOriginalFile, ByteLength: int64(len(data)), SHA256: providerutil.SHA256Hex(data), CapabilityRecordChecksum: strings.Repeat("a", 64), ProviderMetadataChecksum: strings.Repeat("b", 64)}
}

func understandingAuthorization(p *Provider, meta document.AuthorizedUploadMetadata) document.RenditionAuthorization {
	now := time.Now().UTC()
	return document.RenditionAuthorization{ProviderID: p.descriptor.ID, DescriptorFingerprint: p.descriptor.Fingerprint, PolicyFingerprint: p.descriptor.PolicyFingerprint, RenditionRequestFingerprint: strings.Repeat("c", 64), SourceSHA256: meta.SHA256, SourceBytes: meta.ByteLength, CapabilityRecordChecksum: meta.CapabilityRecordChecksum, ProviderMetadataChecksum: meta.ProviderMetadataChecksum, MediaFamily: meta.MediaFamily, MediaType: meta.MediaType, InputKind: meta.InputKind, AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}, MaxArtifactBytes: 1 << 20, MaxArtifacts: 1, MaxTotalResultBytes: 1 << 20, AuthorizedAt: now.Format("2006-01-02T15:04:05.000000000Z"), ExpiresAt: now.Add(time.Minute).Format("2006-01-02T15:04:05.000000000Z")}
}

func TestUnderstandingRejectsInvalidImageBounds(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	p, err := New(Profile{DescriptorID: UnderstandingProviderID, Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
	require.NoError(t, err)
	data := []byte("synthetic-image")
	for _, tc := range []struct {
		name   string
		data   []byte
		mutate func(*document.AuthorizedUploadMetadata)
		want   string
	}{
		{"oversized", bytes.Repeat([]byte("x"), 16<<20+1), func(meta *document.AuthorizedUploadMetadata) {}, "image at most 16 MiB"},
		{"unsupported format", data, func(meta *document.AuthorizedUploadMetadata) { meta.MediaType = "image/gif" }, "unsupported format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := understandingMetadata(tc.data)
			tc.mutate(&meta)
			_, err := document.RenderRendition(t.Context(), p, &testUpload{ReadCloser: io.NopCloser(bytes.NewReader(tc.data)), metadata: meta}, understandingAuthorization(p, meta))
			require.ErrorContains(t, err, tc.want)
			assert.Zero(t, calls.Load())
		})
	}
}
