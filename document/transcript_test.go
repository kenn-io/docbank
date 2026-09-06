package document

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildTranscriptEvidenceV1(t *testing.T) {
	policy, err := NewEvidencePolicy(100)
	require.NoError(t, err)

	evidence, artifact, err := BuildTranscriptEvidenceV1(SuppliedTranscript{
		Provider: "beeper",
		Text:     "The shipment arrives at dock seven.",
	}, policy)
	require.NoError(t, err)

	assert.Equal(t, EvidenceDegradedProvenance, evidence.Completeness)
	assert.Equal(t, "audio", evidence.Family)
	assert.Equal(t, EvidenceUnitGeneric, evidence.UnitKind)
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

func TestBuildTranscriptEvidenceV1UsesCanonicalPolicyBoundary(t *testing.T) {
	policy, err := NewEvidencePolicy(4)
	require.NoError(t, err)

	evidence, artifact, err := BuildTranscriptEvidenceV1(SuppliedTranscript{
		Provider: "beeper", Text: "Café",
	}, policy)
	require.NoError(t, err)
	assert.Equal(t, "Café", evidence.Units[0].Text)
	assert.Contains(t, string(artifact.Payload), "Café")

	_, _, err = BuildTranscriptEvidenceV1(SuppliedTranscript{
		Provider: "beeper", Text: "Café!",
	}, policy)
	require.ErrorContains(t, err, "character budget")
}

func TestBuildTranscriptEvidenceV1RejectsInvalidInput(t *testing.T) {
	policy, err := NewEvidencePolicy(4)
	require.NoError(t, err)

	for name, input := range map[string]SuppliedTranscript{
		"blank":            {Provider: "beeper", Text: " \n\t"},
		"invalid provider": {Provider: "Beeper", Text: "words"},
		"invalid UTF-8":    {Provider: "beeper", Text: string([]byte{0xff})},
		"NUL":              {Provider: "beeper", Text: "a\x00b"},
		"over policy":      {Provider: "beeper", Text: "12345"},
	} {
		t.Run(name, func(t *testing.T) {
			evidence, artifact, err := BuildTranscriptEvidenceV1(input, policy)
			require.Error(t, err)
			assert.Empty(t, evidence)
			assert.Empty(t, artifact)
		})
	}
}

func TestBuildTranscriptEvidenceV1KeepsArtifactIdentitySeparateFromNormalizedText(t *testing.T) {
	policy, err := NewEvidencePolicy(100)
	require.NoError(t, err)

	first, firstArtifact, err := BuildTranscriptEvidenceV1(SuppliedTranscript{
		Provider: "beeper", Text: "Cafe\u0301\r\narrival",
	}, policy)
	require.NoError(t, err)
	second, secondArtifact, err := BuildTranscriptEvidenceV1(SuppliedTranscript{
		Provider: "other", Text: "Café\narrival",
	}, policy)
	require.NoError(t, err)

	assert.Equal(t, "Café\narrival", first.Units[0].Text)
	assert.NotEqual(t, firstArtifact.SHA256, secondArtifact.SHA256)
	assert.NotEqual(t, first.Checksum, second.Checksum)
	var payload struct {
		Provider string `json:"provider"`
		Text     string `json:"text"`
	}
	require.NoError(t, json.Unmarshal(firstArtifact.Payload, &payload))
	assert.Equal(t, "beeper", payload.Provider)
	assert.Equal(t, "Café\r\narrival", payload.Text)
}
