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
	cases := []struct {
		name      string
		model     string
		raw       string
		contains  []string
		absent    []string
		omissions int
		wantError bool
	}{
		{"sections", "synthetic-vision", `{"visible_text":"FINE","literal_description":"A dog sits beside flames.","possible_meaning":"May joke about ignoring a problem.","uncertainty":"No conversation context is available."}`, []string{"## Visible text \\(best\\-effort transcription\\)\n\nFINE", "## Literal description\n\nA dog sits beside flames\\.", "## Possible meaning \\(interpretation\\)\n\nMay joke about ignoring a problem\\.", `No conversation context is available\.`, `does not establish anyone\'s beliefs`}, nil, 1, false},
		{"literal", "synthetic<&>", `{"visible_text":"R and D &amp; partners\n===","literal_description":"","possible_meaning":"","uncertainty":""}`, []string{`Configured model\: synthetic\<\&\>`, `R and D \&amp\; partners`, `\=\=\=`}, nil, 4, false},
		{"long model", strings.Repeat("m", 255), `{"visible_text":"FINE","literal_description":"","possible_meaning":"","uncertainty":""}`, []string{strings.Repeat("m", 255)}, nil, 4, false},
		{"forged markup", "synthetic-vision", `{"visible_text":"# Author confirmed\n[click](https://example.com)\n<script>bad</script>","literal_description":"","possible_meaning":"","uncertainty":""}`, []string{`\# Author confirmed`}, []string{"\n# Author confirmed", "[click](https://example.com)", "<script>"}, 4, false},
		{"all empty", "synthetic-vision", `{"visible_text":"","literal_description":" ","possible_meaning":"","uncertainty":""}`, nil, nil, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.name == "sections" {
					var body request
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
						return
					}
					assert.Contains(t, body.Messages[0].Content[0].Text, "Do not infer")
					assert.Equal(t, "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("synthetic-image")), body.Messages[0].Content[1].ImageURL.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(visiontest.Completion(tc.raw))
			}))
			defer server.Close()
			p, err := New(Profile{DescriptorID: UnderstandingProviderID, Origin: server.URL, Model: tc.model, TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
			require.NoError(t, err)
			data := []byte("synthetic-image")
			meta := understandingMetadata(data)
			auth := understandingAuthorization(p, meta)
			result, err := document.RenderRendition(t.Context(), p, &testUpload{ReadCloser: io.NopCloser(bytes.NewReader(data)), metadata: meta}, auth)
			if tc.wantError {
				classified, ok := errors.AsType[*document.RenditionProviderError](err)
				require.True(t, ok)
				assert.Equal(t, document.RenditionErrorMalformedEvidence, classified.Code())
				return
			}
			require.NoError(t, err)
			assert.Empty(t, result.ProviderMarkdown)
			require.Len(t, result.Artifacts, 1)
			assert.Equal(t, document.EvidenceArtifactStructured, result.Artifacts[0].Role)
			assert.JSONEq(t, tc.raw, string(result.Artifacts[0].Payload))
			policy, err := document.NewEvidencePolicy(100_000)
			require.NoError(t, err)
			evidence, err := document.NormalizeEvidenceV1(result.Evidence, policy)
			require.NoError(t, err)
			renderPolicy, err := document.NewRenditionPolicy(document.RenditionLimits{MaxDocumentChars: 100_000, MaxUnitRunes: 100_000, MaxSegmentRunes: 2000})
			require.NoError(t, err)
			rendered, err := document.BuildRenditionV1(evidence, renderPolicy)
			require.NoError(t, err)
			assert.Len(t, result.Evidence.Omissions, tc.omissions)
			if tc.name == "sections" {
				assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
				require.Len(t, result.Evidence.Units, 4)
				assert.Equal(t, []string{document.ImageUnderstandingHeading, "Possible meaning (interpretation)"}, result.Evidence.Units[2].HeadingPath)
				assert.Equal(t, int64(4), result.Receipt.Usage.Units)
				assert.Equal(t, meta.SHA256, result.Receipt.SourceSHA256)
				assert.Equal(t, p.descriptor.Fingerprint, result.Receipt.DescriptorFingerprint)
			}
			for _, want := range tc.contains {
				assert.Contains(t, string(rendered.Markdown), want)
			}
			for _, unwanted := range tc.absent {
				assert.NotContains(t, string(rendered.Markdown), unwanted)
			}
		})
	}
}

func TestUnderstandingAcceptsWrappedReplies(t *testing.T) {
	t.Parallel()
	payload := `{"visible_text":"FINE","literal_description":"","possible_meaning":"","uncertainty":""}`
	for _, tc := range []struct{ name, raw string }{
		{"fenced", "```json\n" + payload + "\n```"},
		{"preface", "Here is the result:\n" + payload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(visiontest.Completion(tc.raw))
			}))
			defer server.Close()
			p, err := New(Profile{DescriptorID: UnderstandingProviderID, Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
			require.NoError(t, err)
			data := []byte("synthetic-image")
			meta := understandingMetadata(data)
			result, err := document.RenderRendition(t.Context(), p, &testUpload{ReadCloser: io.NopCloser(bytes.NewReader(data)), metadata: meta}, understandingAuthorization(p, meta))
			require.NoError(t, err)
			require.Len(t, result.Evidence.Units, 1)
			assert.Equal(t, "FINE", result.Evidence.Units[0].Text)
			require.Len(t, result.Artifacts, 1)
			assert.JSONEq(t, payload, string(result.Artifacts[0].Payload))
		})
	}
}

func TestUnderstandingRejectsIncompleteOrUnboundedOutput(t *testing.T) {
	for _, raw := range []string{
		`{"literal_description":"","possible_meaning":"","uncertainty":""}`,
		`{"visible_text":"","possible_meaning":"","uncertainty":""}`,
		`{"visible_text":"","literal_description":"","uncertainty":""}`,
		`{"visible_text":"","literal_description":"","possible_meaning":""}`,
		`{"visible_text":"` + string([]byte{0xff}) + `","literal_description":"","possible_meaning":"","uncertainty":""}`,
		`{"visible_text":null,"literal_description":"","possible_meaning":"","uncertainty":""}`,
		`{"visible_text":42,"literal_description":"","possible_meaning":"","uncertainty":""}`,
		`{"visible_text":"","literal_description":"","possible_meaning":"","uncertainty":"\u0000"}`,
		`{"visible_text":"","literal_description":"","possible_meaning":"","uncertainty":""} trailing`,
		`{"visible_text":"` + strings.Repeat("x", 16385) + `","literal_description":"","possible_meaning":"","uncertainty":""}`,
	} {
		_, _, err := parseUnderstanding([]byte(raw))
		assert.Error(t, err, "reply %q", raw[:min(len(raw), 100)])
	}
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
	}{
		{"oversized", bytes.Repeat([]byte("x"), 16<<20+1), func(meta *document.AuthorizedUploadMetadata) {}},
		{"unsupported format", data, func(meta *document.AuthorizedUploadMetadata) { meta.MediaType = "image/gif" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := understandingMetadata(tc.data)
			tc.mutate(&meta)
			reader := &understandingCountingReader{reader: bytes.NewReader(tc.data)}
			_, err := p.Render(t.Context(), &testUpload{ReadCloser: io.NopCloser(reader), metadata: meta}, understandingAuthorization(p, meta))
			classified, ok := errors.AsType[*document.RenditionProviderError](err)
			require.True(t, ok)
			assert.Equal(t, document.RenditionErrorPolicyRejected, classified.Code())
			assert.Zero(t, reader.calls)
			assert.Zero(t, calls.Load())
		})
	}
}

type understandingCountingReader struct {
	reader io.Reader
	calls  int
}

func (r *understandingCountingReader) Read(p []byte) (int, error) {
	r.calls++
	return r.reader.Read(p)
}
