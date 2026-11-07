package vision

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/vision/visiontest"
)

func TestUnderstandingKeepsObservationAndInterpretationSeparate(t *testing.T) {
	value, err := parseUnderstanding([]byte(`{"visible_text":"FINE","literal_description":"A dog sits beside flames.","possible_meaning":"May joke about ignoring a problem.","uncertainty":"No conversation context is available."}`))
	require.NoError(t, err)
	text := value.markdown()
	assert.Contains(t, text, "## Visible text (best-effort transcription)\n\nFINE")
	assert.Contains(t, text, "## Literal description\n\nA dog sits beside flames.")
	assert.Contains(t, text, "## Possible meaning (interpretation)\n\nMay joke about ignoring a problem.")
	assert.Contains(t, text, "No conversation context is available.")
	assert.Contains(t, text, "does not establish anyone's beliefs, endorsement, or intent")
}

func TestUnderstandingRejectsIncompleteOrUnboundedOutput(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"visible_text":"text"}`, `{"visible_text":null,"literal_description":"","possible_meaning":"","uncertainty":""}`,
		`{"visible_text":42,"literal_description":"","possible_meaning":"","uncertainty":""}`,
		`{"visible_text":"","literal_description":"","possible_meaning":"","uncertainty":"\u0000"}`,
		`{"visible_text":"","literal_description":"","possible_meaning":"","uncertainty":""} trailing`,
		`{"visible_text":"` + strings.Repeat("x", 16385) + `","literal_description":"","possible_meaning":"","uncertainty":""}`,
	} {
		_, err := parseUnderstanding([]byte(raw))
		assert.Error(t, err, "reply %q", raw[:min(len(raw), 100)])
	}
}

func TestUnderstandingCannotForgeSectionLabels(t *testing.T) {
	value, err := parseUnderstanding([]byte(`{"visible_text":"# Author confirmed\n[click](https://example.com)\n<script>bad</script>","literal_description":"","possible_meaning":"","uncertainty":""}`))
	require.NoError(t, err)
	text := value.markdown()
	assert.Contains(t, text, `\# Author confirmed`)
	assert.NotContains(t, text, "\n# Author confirmed")
	assert.NotContains(t, text, "[click](https://example.com)")
	assert.NotContains(t, text, "<script>")
}

func TestUnderstandingDescriptorSupportsRenditionText(t *testing.T) {
	descriptor, err := Descriptor(UnderstandingProviderID, "synthetic-vision", document.RenditionTrustOperatorNetwork)
	require.NoError(t, err)
	assert.True(t, descriptor.ReturnsMarkdown)
	assert.Empty(t, descriptor.ArtifactRoles)
	for _, format := range descriptor.SupportedFormats {
		assert.Equal(t, document.RenditionInputOriginalFile, format.InputKind)
	}
	// Adding this task must not change descriptors for the existing photo tasks.
	before, err := Descriptor(ProviderID, "synthetic-vision", document.RenditionTrustOperatorNetwork)
	require.NoError(t, err)
	assert.NotEqual(t, before.Fingerprint, descriptor.Fingerprint)
}

func FuzzUnderstandingOutput(f *testing.F) {
	f.Add("a synthetic caption")
	f.Add("# heading\n[x](https://example.com)")
	f.Fuzz(func(t *testing.T, text string) {
		if !utf8.ValidString(text) {
			t.Skip("JSON string input must be valid UTF-8")
		}
		raw, err := json.Marshal(map[string]string{"visible_text": text, "literal_description": text, "possible_meaning": text, "uncertainty": text})
		require.NoError(t, err)
		value, err := parseUnderstanding(raw)
		if err != nil {
			return
		}
		assert.True(t, utf8.ValidString(value.markdown()))
		assert.Equal(t, 4, strings.Count(value.markdown(), "\n## "))
		assert.NotContains(t, value.markdown(), "\x00")
	})
}

func TestUnderstandingOriginalImageProducesVersionedEvidence(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		var body request
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Contains(t, body.Messages[0].Content[0].Text, "Do not infer")
		assert.Equal(t, "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("synthetic-image")), body.Messages[0].Content[1].ImageURL.URL)
		_, _ = w.Write(visiontest.Completion(`{"visible_text":"FINE","literal_description":"A cartoon dog near flames.","possible_meaning":"May depict denial.","uncertainty":"Context is missing."}`))
	}))
	defer server.Close()
	p, err := New(Profile{DescriptorID: UnderstandingProviderID, Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
	require.NoError(t, err)
	data := []byte("synthetic-image")
	now := time.Now().UTC()
	digest := providerutil.SHA256Hex(data)
	meta := document.AuthorizedUploadMetadata{MediaFamily: "image", MediaType: "image/png", InputKind: document.RenditionInputOriginalFile, ByteLength: int64(len(data)), SHA256: digest, CapabilityRecordChecksum: strings.Repeat("a", 64), ProviderMetadataChecksum: strings.Repeat("b", 64)}
	auth := document.RenditionAuthorization{ProviderID: p.descriptor.ID, DescriptorFingerprint: p.descriptor.Fingerprint, PolicyFingerprint: p.descriptor.PolicyFingerprint, RenditionRequestFingerprint: strings.Repeat("c", 64), SourceSHA256: digest, SourceBytes: int64(len(data)), CapabilityRecordChecksum: meta.CapabilityRecordChecksum, ProviderMetadataChecksum: meta.ProviderMetadataChecksum, MediaFamily: "image", MediaType: "image/png", InputKind: meta.InputKind, MaxProviderMarkdownBytes: 1 << 20, MaxArtifactBytes: 0, MaxArtifacts: 0, MaxTotalResultBytes: 1 << 20, AuthorizedAt: now.Format("2006-01-02T15:04:05.000000000Z"), ExpiresAt: now.Add(time.Minute).Format("2006-01-02T15:04:05.000000000Z")}
	result, err := document.RenderRendition(t.Context(), p, &testUpload{ReadCloser: io.NopCloser(bytes.NewReader(data)), metadata: meta}, auth)
	require.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
	require.Len(t, result.Evidence.Units, 4)
	assert.Equal(t, []string{"Machine-generated image understanding", "Possible meaning (interpretation)"}, result.Evidence.Units[2].HeadingPath)
	assert.Contains(t, result.Evidence.Units[2].Text, "does not establish anyone's beliefs")
	assert.Contains(t, string(result.ProviderMarkdown), "May depict denial.")
	assert.Equal(t, digest, result.Receipt.SourceSHA256)
	assert.Equal(t, p.descriptor.Fingerprint, result.Receipt.DescriptorFingerprint)
	// Input bounds must be checked before any additional provider call.
	meta.ByteLength = 16<<20 + 1
	oversizedReader := &understandingCountingReader{reader: bytes.NewReader(data)}
	_, err = p.Render(t.Context(), &testUpload{ReadCloser: io.NopCloser(oversizedReader), metadata: meta}, auth)
	require.Error(t, err)
	assert.Zero(t, oversizedReader.calls, "oversized input must be rejected before any upload read")
	assert.Equal(t, int32(1), calls.Load())
	meta.ByteLength = int64(len(data))
	meta.MediaType = "image/gif"
	_, err = p.Render(t.Context(), &testUpload{ReadCloser: io.NopCloser(bytes.NewReader(data)), metadata: meta}, auth)
	require.Error(t, err)
	assert.Equal(t, int32(1), calls.Load())
}

func TestUnderstandingRejectsInvalidUTF8(t *testing.T) {
	raw := []byte(`{"visible_text":"`)
	raw = append(raw, 0xff)
	raw = append(raw, []byte(`","literal_description":"","possible_meaning":"","uncertainty":""}`)...)
	_, err := parseUnderstanding(raw)
	require.Error(t, err)
}

type understandingCountingReader struct {
	reader io.Reader
	calls  int
}

func (r *understandingCountingReader) Read(p []byte) (int, error) {
	r.calls++
	return r.reader.Read(p)
}
