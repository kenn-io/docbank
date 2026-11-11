package vision

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
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

// shown renders evidence units as readers see them.
func shown(evidence document.SourceEvidenceV1) string {
	var out strings.Builder
	for i, unit := range evidence.Units {
		out.WriteString(document.ImageUnderstandingMarkdown(unit.HeadingPath, unit.Speaker, unit.Text, i == 0) + "\n\n")
	}
	return out.String()
}

func TestUnderstandingKeepsObservationAndInterpretationSeparate(t *testing.T) {
	value, err := parseUnderstanding([]byte(`{"visible_text":"FINE","literal_description":"A dog sits beside flames.","possible_meaning":"May joke about ignoring a problem.","uncertainty":"No conversation context is available."}`))
	require.NoError(t, err)
	evidence := value.evidence("synthetic-vision")
	require.Len(t, evidence.Units, 4)
	var indexed string
	for _, unit := range evidence.Units {
		indexed += unit.Text + "\n"
		assert.Equal(t, document.ImageUnderstandingHeading, unit.HeadingPath[0])
	}
	assert.Equal(t, "FINE\nA dog sits beside flames.\nMay joke about ignoring a problem.\nNo conversation context is available.\n", indexed)
	text := shown(evidence)
	assert.Contains(t, text, "## Visible text \\(best\\-effort transcription\\)\n\nFINE")
	assert.Contains(t, text, "## Literal description\n\nA dog sits beside flames\\.")
	assert.Contains(t, text, "## Possible meaning \\(interpretation\\)\n\nMay joke about ignoring a problem\\.")
	assert.Contains(t, text, "No conversation context is available\\.")
	assert.Contains(t, text, `does not establish anyone\'s beliefs\, endorsement\, or intent`)
}

func TestUnderstandingPreservesLiteralMarkdownText(t *testing.T) {
	value, err := parseUnderstanding([]byte(`{"visible_text":"R and D &amp; partners\n===","literal_description":"","possible_meaning":"","uncertainty":""}`))
	require.NoError(t, err)
	evidence := value.evidence("synthetic<&>")
	require.Len(t, evidence.Units, 1)
	assert.Equal(t, "R and D &amp; partners\n===", evidence.Units[0].Text)
	text := shown(evidence)
	assert.Contains(t, text, `Configured model\: synthetic\<\&\>`)
	assert.Contains(t, text, `R and D \&amp\; partners`)
	assert.Contains(t, text, `\=\=\=`)
	assert.Len(t, evidence.Omissions, 4)
}

func TestUnderstandingRejectsIncompleteOrUnboundedOutput(t *testing.T) {
	for _, raw := range []string{
		`{"literal_description":"","possible_meaning":"","uncertainty":""}`,
		`{"visible_text":"","possible_meaning":"","uncertainty":""}`,
		`{"visible_text":"","literal_description":"","uncertainty":""}`,
		`{"visible_text":"","literal_description":"","possible_meaning":""}`,
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

func TestUnderstandingRetainsLongConfiguredModel(t *testing.T) {
	value, err := parseUnderstanding([]byte(`{"visible_text":"FINE","literal_description":"","possible_meaning":"","uncertainty":""}`))
	require.NoError(t, err)
	policy, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)
	for _, length := range []int{129, 255} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			model := strings.Repeat("m", length)
			_, err := Descriptor(UnderstandingProviderID, model, document.RenditionTrustOperatorNetwork)
			require.NoError(t, err)
			normalized, err := document.NormalizeEvidenceV1(value.evidence(model), policy)
			require.NoError(t, err)
			renderPolicy, err := document.NewRenditionPolicy(document.RenditionLimits{MaxDocumentChars: 10_000, MaxUnitRunes: 10_000, MaxSegmentRunes: 1_000})
			require.NoError(t, err)
			rendered, err := document.BuildRenditionV1(normalized, renderPolicy)
			require.NoError(t, err)
			assert.Contains(t, string(rendered.Markdown), model)
		})
	}
}

func TestUnderstandingCannotForgeSectionLabels(t *testing.T) {
	value, err := parseUnderstanding([]byte(`{"visible_text":"# Author confirmed\n[click](https://example.com)\n<script>bad</script>","literal_description":"","possible_meaning":"","uncertainty":""}`))
	require.NoError(t, err)
	text := shown(value.evidence("synthetic-vision"))
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
	assert.Equal(t, []string{"Machine-generated image understanding; unverified; sender intent unknown", "Possible meaning (interpretation)"}, result.Evidence.Units[2].HeadingPath)
	assert.Equal(t, int64(4), result.Receipt.Usage.Units)
	assert.Contains(t, string(result.ProviderMarkdown), "May depict denial\\.")
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
