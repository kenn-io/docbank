package document_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestBuildSuppliedTextSourceEvidenceV1(t *testing.T) {
	policy, err := document.NewEvidencePolicy(100)
	require.NoError(t, err)
	input := document.SuppliedText{Provider: "package", Text: "Cafe\u0301\r\nquenchwood"}
	source, artifact, err := document.BuildSuppliedTextSourceEvidenceV1(input, policy)
	require.NoError(t, err)
	require.Equal(t, document.EvidenceDegradedProvenance, source.Completeness)
	require.Equal(t, document.EvidenceUnitGeneric, source.UnitKind)
	require.Equal(t, input.Text, source.Units[0].Text)
	require.Equal(t, document.EvidenceArtifactStructured, artifact.Role)
	require.Equal(t, artifact.SHA256, source.Artifacts[0].SHA256)
	digest := sha256.Sum256(artifact.Payload)
	require.Equal(t, hex.EncodeToString(digest[:]), artifact.SHA256)
	var retained struct {
		ContractVersion string `json:"contract_version"`
		Provider        string `json:"provider"`
		Text            string `json:"text"`
	}
	require.NoError(t, json.Unmarshal(artifact.Payload, &retained))
	require.Equal(t, "supplied-text/v1", retained.ContractVersion)
	require.Equal(t, input.Text, retained.Text)
	_, err = document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
}

func TestBuildSuppliedTextSourceEvidenceV1RejectsInvalidText(t *testing.T) {
	policy, err := document.NewEvidencePolicy(4)
	require.NoError(t, err)
	for _, input := range []document.SuppliedText{
		{Provider: "package", Text: " \n"},
		{Provider: "Package", Text: "word"},
		{Provider: "package", Text: string([]byte{0xff})},
		{Provider: "package", Text: "12345"},
	} {
		_, artifact, err := document.BuildSuppliedTextSourceEvidenceV1(input, policy)
		require.Error(t, err)
		require.Empty(t, artifact)
	}
}
