package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/canonical"
)

func TestMediaSourceVersionSelectsTheExactVisibleRevision(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "first.wav", fakeHash("a1"), 11, "audio/wav")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "second.wav", fakeHash("b2"), 12, "audio/wav")
	require.NoError(t, err)
	sourceID := strings.Repeat("a", 64)
	_, err = s.db.Exec(`INSERT INTO media_sources VALUES(?,?,?,?,?,?)`, sourceID, "supplied_media", "", "", sourceID, nowRFC3339())
	require.NoError(t, err)
	require.NoError(t, declareTestOccurrence(ctx, s, MediaOccurrenceInput{
		ID: "occurrence-a", SourceID: sourceID, Principal: "operator:test", Ref: "call-a", Revision: "1", MessageJSON: "{}",
	}))
	require.NoError(t, s.PublishMediaSourceVersion(ctx, MediaSourceVersionInput{
		ID: "source-version-a", SourceID: sourceID, Revision: 1, ContentVersionID: first.CurrentVersionID,
		CaptureJSON: "{}", BindOccurrenceIDs: []string{"occurrence-a"},
	}))
	require.NoError(t, declareTestOccurrence(ctx, s, MediaOccurrenceInput{
		ID: "occurrence-b", SourceID: sourceID, Principal: "operator:test", Ref: "call-b", Revision: "1", MessageJSON: "{}",
	}))
	require.NoError(t, s.PublishMediaSourceVersion(ctx, MediaSourceVersionInput{
		ID: "source-version-b", SourceID: sourceID, Revision: 2, ContentVersionID: second.CurrentVersionID,
		CaptureJSON: "{}", ExpectedHeadRevision: 1, BindOccurrenceIDs: []string{"occurrence-b"},
	}))
	for _, receipt := range []struct {
		versionID, contentVersionID, occurrenceID, operationID, requestHash, createdAt string
	}{
		{"", first.CurrentVersionID, "occurrence-a", "operation-retention", strings.Repeat("c", 64), "2023-01-01T00:00:00Z"},
		{"source-version-a", first.CurrentVersionID, "occurrence-a", "operation-a", strings.Repeat("a", 64), "2024-01-01T00:00:00Z"},
		{"source-version-b", second.CurrentVersionID, "occurrence-b", "operation-b", strings.Repeat("b", 64), "2024-01-02T00:00:00Z"},
	} {
		publication := MediaPublicationReceipt{VaultUID: s.VaultID(), SourceID: sourceID,
			SourceVersionID: receipt.versionID, ContentVersionID: receipt.contentVersionID,
			OccurrenceID: receipt.occurrenceID, OperationID: receipt.operationID,
			OperationState: "succeeded", CoverageState: "unprocessed"}
		raw, err := canonical.Marshal(publication)
		require.NoError(t, err)
		_, err = s.db.Exec(`INSERT INTO media_operations(operation_id,principal,verb,request_sha256,source_id,receipt_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			receipt.operationID, "operator:test", "submit_supplied_media", receipt.requestHash, sourceID, string(raw), receipt.createdAt, receipt.createdAt)
		require.NoError(t, err)
	}

	exact, err := s.MediaSourceVersion(ctx, "operator:test", sourceID, "source-version-a")
	require.NoError(t, err)
	require.Equal(t, "source-version-a", exact.SourceVersionID)
	require.Equal(t, first.CurrentVersionID, exact.ContentVersionID)
	require.Equal(t, "source-version-a", exact.Receipt.SourceVersionID)
	keys := []MediaSourceVersionKey{{sourceID, "source-version-a"}, {sourceID, "source-version-b"}, {sourceID, "hidden-version"}}
	batch, err := s.MediaSourceVersions(ctx, "operator:test", keys)
	require.NoError(t, err)
	require.Len(t, batch, 2)
	require.Equal(t, exact, batch[keys[0]])
	secondExact, err := s.MediaSourceVersion(ctx, "operator:test", sourceID, "source-version-b")
	require.NoError(t, err)
	require.Equal(t, secondExact, batch[keys[1]])

	latest, err := s.MediaSource(ctx, "operator:test", sourceID)
	require.NoError(t, err)
	require.Equal(t, "source-version-b", latest.SourceVersionID)
	require.Equal(t, "source-version-b", latest.Receipt.SourceVersionID)
	page, total, more, err := s.MediaSources(ctx, "operator:test", "", 10)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.False(t, more)
	require.Equal(t, []MediaSourceProjection{latest}, page)
	_, err = s.MediaSourceVersion(ctx, "operator:test", sourceID, "hidden-version")
	require.ErrorIs(t, err, ErrNotFound)
}
