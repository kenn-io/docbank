package bridge

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestBridgeOrdinaryEvidenceWithImageHeading(t *testing.T) {
	t.Parallel()
	fixture := newBridgeFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envelope := completedEnvelope(t, fixture, "job-heading", nil)
		evidence := document.SourceEvidenceV1{
			ContractVersion: document.SourceEvidenceContractV1,
			Completeness:    document.EvidenceDegradedProvenance,
			Family:          fixture.authorization.MediaFamily, UnitKind: document.EvidenceUnitGeneric,
			Omissions: []document.SourceEvidenceOmissionV1{{Kind: document.EvidenceOmissionField, Field: "natural_provenance", Reason: "generic source"}},
			Units:     []document.SourceEvidenceUnitV1{{Order: 0, HeadingPath: []string{document.ImageUnderstandingHeading, "Example"}, Text: "[link](javascript:alert(1)) <iframe>synthetic</iframe>", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone}}},
		}
		data, err := json.Marshal(evidence, json.Deterministic(true))
		if !assert.NoError(t, err) {
			return
		}
		envelope["result"].(map[string]any)["evidence"] = map[string]any{"media_type": evidenceMediaType, "byte_length": len(data), "sha256": sha256String(data), "inline": jsontext.Value(data)}
		writeBridgeJSON(t, w, http.StatusOK, envelope)
	}))
	t.Cleanup(server.Close)
	client := newTestBridgeClient(t, server.URL, fixture.descriptor, nil)
	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	ep, err := document.NewEvidencePolicy(10000)
	require.NoError(t, err)
	evidence, err := document.NormalizeEvidenceV1(result.Evidence, ep)
	require.NoError(t, err)
	rp, err := document.NewRenditionPolicy(document.RenditionLimits{MaxDocumentChars: 10000, MaxUnitRunes: 1000, MaxSegmentRunes: 100})
	require.NoError(t, err)
	rendition, err := document.BuildRenditionV1(evidence, rp)
	require.NoError(t, err)
	assert.NotContains(t, string(rendition.Markdown), "model interpretation")
	assert.NotContains(t, rendition.Units[0].Text, "javascript:")
	hash := strings.Repeat("a", 64)
	enveloped, _, err := document.EnvelopeRenditionV1(rendition, document.RenditionEnvelopeV1{BuildID: hash, SourceSHA256: hash, SourceFormat: "pdf", SourceMediaType: "application/pdf", RenditionRequestFingerprint: hash, EvidenceLexicalFingerprint: hash, NormalizedEvidenceContract: document.NormalizedEvidenceContractV1, UnitKind: document.EvidenceUnitGeneric})
	require.NoError(t, err)
	front, body, err := document.ParseRenditionFrontMatterV1(enveloped.Markdown)
	require.NoError(t, err)
	require.Len(t, front.Navigation.Entries, 1)
	assert.True(t, strings.HasPrefix(string(body[front.Navigation.Entries[0].Byte:]), rendition.Units[0].Text))
}
