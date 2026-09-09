package document_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestBuildTranscriptEvidenceV1(t *testing.T) {
	policy, err := document.NewEvidencePolicy(100)
	require.NoError(t, err)

	evidence, artifact, err := document.BuildTranscriptEvidenceV1(document.SuppliedTranscript{
		Provider: "beeper",
		Text:     "The shipment arrives at dock seven.",
	}, policy)
	require.NoError(t, err)

	assert.Equal(t, document.EvidenceDegradedProvenance, evidence.Completeness)
	assert.Equal(t, "audio", evidence.Family)
	assert.Equal(t, document.EvidenceUnitGeneric, evidence.UnitKind)
	require.Len(t, evidence.Units, 1)
	assert.Equal(t, "The shipment arrives at dock seven.", evidence.Units[0].Text)
	require.Len(t, evidence.Omissions, 1)
	assert.Equal(t, "natural_provenance", evidence.Omissions[0].Field)
	assert.Contains(t, evidence.Omissions[0].Reason, "timing")
	require.Len(t, evidence.Artifacts, 1)
	assert.Equal(t, artifact.SHA256, evidence.Artifacts[0].SHA256)
	assert.Equal(t, "transcript.json", evidence.Artifacts[0].Pointer)

	var payload struct {
		ContractVersion string `json:"contract_version"`
		Provider        string `json:"provider"`
		Text            string `json:"text"`
	}
	require.NoError(t, json.Unmarshal(artifact.Payload, &payload))
	assert.Equal(t, "supplied-transcript/v1", payload.ContractVersion)
	assert.Equal(t, "beeper", payload.Provider)
	assert.Equal(t, "The shipment arrives at dock seven.", payload.Text)
}

func TestBuildTranscriptSourceEvidenceV1MatchesNormalizedHelperSource(t *testing.T) {
	policy, err := document.NewEvidencePolicy(100)
	require.NoError(t, err)
	transcript := document.SuppliedTranscript{Provider: "beeper", Text: "The shipment arrives at dock seven."}

	source, sourceArtifact, err := document.BuildTranscriptSourceEvidenceV1(transcript, policy)
	require.NoError(t, err)
	normalized, normalizedArtifact, err := document.BuildTranscriptEvidenceV1(transcript, policy)
	require.NoError(t, err)

	expected, err := document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
	assert.Equal(t, expected, normalized)
	assert.Equal(t, sourceArtifact, normalizedArtifact)
	assert.Equal(t, sourceArtifact.Payload, normalizedArtifact.Payload)
}

func TestBuildTranscriptEvidenceV1UsesCanonicalPolicyBoundary(t *testing.T) {
	policy, err := document.NewEvidencePolicy(4)
	require.NoError(t, err)

	evidence, artifact, err := document.BuildTranscriptEvidenceV1(document.SuppliedTranscript{
		Provider: "beeper", Text: "Café",
	}, policy)
	require.NoError(t, err)
	assert.Equal(t, "Café", evidence.Units[0].Text)
	assert.Contains(t, string(artifact.Payload), "Café")

	_, _, err = document.BuildTranscriptEvidenceV1(document.SuppliedTranscript{
		Provider: "beeper", Text: "Café!",
	}, policy)
	require.ErrorContains(t, err, "character budget")
}

func TestBuildTranscriptEvidenceV1RejectsInvalidInput(t *testing.T) {
	evidence, artifact, err := document.BuildTranscriptEvidenceV1(document.SuppliedTranscript{
		Provider: "beeper", Text: "words",
	}, document.EvidencePolicy{})
	require.ErrorContains(t, err, "use NewEvidencePolicy")
	assert.Empty(t, evidence)
	assert.Empty(t, artifact)

	policy, err := document.NewEvidencePolicy(4)
	require.NoError(t, err)

	for name, input := range map[string]document.SuppliedTranscript{
		"blank":            {Provider: "beeper", Text: " \n\t"},
		"invalid provider": {Provider: "Beeper", Text: "word"},
		"invalid UTF-8":    {Provider: "beeper", Text: string([]byte{0xff})},
		"NUL":              {Provider: "beeper", Text: "a\x00b"},
		"over policy":      {Provider: "beeper", Text: "12345"},
	} {
		t.Run(name, func(t *testing.T) {
			evidence, artifact, err := document.BuildTranscriptEvidenceV1(input, policy)
			require.Error(t, err)
			assert.Empty(t, evidence)
			assert.Empty(t, artifact)
		})
	}
}

func TestBuildTranscriptEvidenceV1KeepsArtifactIdentitySeparateFromNormalizedText(t *testing.T) {
	policy, err := document.NewEvidencePolicy(100)
	require.NoError(t, err)

	first, firstArtifact, err := document.BuildTranscriptEvidenceV1(document.SuppliedTranscript{
		Provider: "beeper", Text: "Cafe\u0301\r\narrival",
	}, policy)
	require.NoError(t, err)
	repeat, repeatArtifact, err := document.BuildTranscriptEvidenceV1(document.SuppliedTranscript{
		Provider: "beeper", Text: "Cafe\u0301\r\narrival",
	}, policy)
	require.NoError(t, err)
	second, secondArtifact, err := document.BuildTranscriptEvidenceV1(document.SuppliedTranscript{
		Provider: "other", Text: "Café\narrival",
	}, policy)
	require.NoError(t, err)

	assert.Equal(t, "Café\narrival", first.Units[0].Text)
	assert.Equal(t, firstArtifact.SHA256, repeatArtifact.SHA256)
	assert.Equal(t, first.Checksum, repeat.Checksum)
	assert.NotEqual(t, firstArtifact.SHA256, secondArtifact.SHA256)
	assert.NotEqual(t, first.Checksum, second.Checksum)
	digest := sha256.Sum256(firstArtifact.Payload)
	assert.Equal(t, hex.EncodeToString(digest[:]), firstArtifact.SHA256)
	var payload struct {
		Provider string `json:"provider"`
		Text     string `json:"text"`
	}
	require.NoError(t, json.Unmarshal(firstArtifact.Payload, &payload))
	assert.Equal(t, "beeper", payload.Provider)
	assert.Equal(t, "Café\r\narrival", payload.Text)
}
