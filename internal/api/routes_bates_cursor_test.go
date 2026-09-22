package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestBatesCandidateCursorsAreSignedSelectorBoundAndSupportEvidenceContinuation(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	service := newDocumentCursorTestService(&now)
	position := store.BatesArtifactPosition{CreatedAt: now.Format(time.RFC3339Nano),
		ArtifactID: "11111111-1111-4111-8111-111111111111"}
	cursor, err := encodeBatesCandidateCursor(service, "bates_label", "OUR000001", 0, position)
	require.NoError(t, err)
	decoded, evidenceID, offset, epoch, err := decodeBatesCandidateCursor(service, cursor, "bates_label", "OUR000001")
	require.NoError(t, err)
	require.Equal(t, position, decoded)
	require.Empty(t, evidenceID)
	require.Zero(t, offset)
	require.Zero(t, epoch)
	invalidPosition, invalidEvidenceID, invalidOffset, invalidEpoch, err := decodeBatesCandidateCursor(
		service, cursor, "bates_label", "OTHER000001")
	require.Error(t, err)
	require.Zero(t, invalidPosition)
	require.Empty(t, invalidEvidenceID)
	require.Zero(t, invalidOffset)
	require.Zero(t, invalidEpoch)
	tampered := cursor[:len(cursor)-1] + "A"
	invalidPosition, invalidEvidenceID, invalidOffset, invalidEpoch, err = decodeBatesCandidateCursor(
		service, tampered, "bates_label", "OUR000001")
	require.Error(t, err)
	require.Zero(t, invalidPosition)
	require.Empty(t, invalidEvidenceID)
	require.Zero(t, invalidOffset)
	require.Zero(t, invalidEpoch)

	evidenceCursor, err := encodeBatesEvidenceCursor(service, "custodian_label", "synthetic keeper", 7,
		position.ArtifactID, 25)
	require.NoError(t, err)
	decoded, evidenceID, offset, epoch, err = decodeBatesCandidateCursor(service, evidenceCursor,
		"custodian_label", "synthetic keeper")
	require.NoError(t, err)
	require.Zero(t, decoded)
	require.Equal(t, position.ArtifactID, evidenceID)
	require.Equal(t, 25, offset)
	require.EqualValues(t, 7, epoch)
}
