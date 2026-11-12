package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func checkDocumentDateKeysFollowGeneration(t *testing.T, open func(*testing.T) *Store) {
	s := open(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "date-fixture.txt", fakeHash("a1"), 10, "text/plain")
	require.NoError(t, err)
	require.ErrorIs(t, requireDocumentDateIndex(ctx, s.db), ErrDocumentDatesPending)
	target := requireDocumentEventTarget(t, s, node.CurrentVersionID)
	record := documentEventRecord(t, s.VaultID(), node.CurrentVersionID, "d1")
	record.Events[0].Actors[0].Sensitive = true
	generation, err := s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)
	var civil string
	var sensitive int
	err = s.db.QueryRowContext(ctx, `SELECT civil_date,sensitive FROM document_date_keys WHERE generation_id=$1`, generation.GenerationID).Scan(&civil, &sensitive)
	require.NoError(t, err)
	require.Equal(t, "2019-03-04", civil)
	require.Equal(t, 1, sensitive)
	require.NoError(t, requireDocumentDateIndex(ctx, s.db))
	_, err = s.db.ExecContext(ctx, `UPDATE document_date_keys SET sensitive=0 WHERE generation_id=$1`, generation.GenerationID)
	require.Error(t, err)
	_, err = s.db.ExecContext(ctx, `DELETE FROM document_event_heads WHERE generation_id=$1`, generation.GenerationID)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `DELETE FROM document_event_generations WHERE generation_id=$1`, generation.GenerationID)
	require.NoError(t, err)
	var count int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_date_keys WHERE generation_id=$1`, generation.GenerationID).Scan(&count))
	require.Zero(t, count)
}
