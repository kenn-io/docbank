package store

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func checkDocumentDateKeysFollowGeneration(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "date-fixture.txt", fakeHash("a1"), 10, "text/plain")
	require.NoError(t, err)
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
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := s.invalidateDocumentEventsForVersionsTx(ctx, tx, []string{node.CurrentVersionID})
		return err
	}))
	var count int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_date_keys WHERE generation_id=$1`, generation.GenerationID).Scan(&count))
	require.Zero(t, count)
}

// A terminal rebuild failure may retain an old head, but no date consumer may use it.
func checkDocumentDateFailedHeadIsNotVisible(t *testing.T, open func(*testing.T) *Store, postgres bool) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "dated.txt", fakeHash("b1"), 1, "text/plain")
	require.NoError(t, err)
	target := requireDocumentEventTarget(t, s, node.CurrentVersionID)
	record := documentEventRecord(t, s.VaultID(), node.CurrentVersionID, "b2")
	_, err = s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)
	var visible int
	read := func() {
		t.Helper()
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_date_keys k JOIN document_event_heads h ON h.generation_id=k.generation_id WHERE h.content_version_id=$1 AND `+currentDocumentDateHead, node.CurrentVersionID).Scan(&visible)
		require.NoError(t, err)
	}
	read()
	require.Equal(t, 1, visible)
	for i, state := range []string{"failed", "unavailable"} {
		_, err = s.StartDocumentEventRebuild(ctx, fmt.Sprintf("90000000-0000-4000-8000-%012d", i+1), DocumentEventRebuildRequestSHA256)
		require.NoError(t, err)
		target = requireDocumentEventTarget(t, s, node.CurrentVersionID)
		require.NoError(t, s.RecordDocumentEventAttempt(ctx, target, requireDocumentEventInputsSHA256(t, s, target), state, []byte(`[]`)))
		require.NoError(t, requireDocumentDateIndex(ctx, s.db))
		read()
		require.Zero(t, visible, state)
		if !postgres {
			service := NewQuerySnapshotService(s)
			page, err := service.Create(ctx, "owner", SnapshotRequest{Query: snapshotTestQuery(t, `{"syntax":"advanced","text":"date:2019-03-04"}`)})
			require.NoError(t, err)
			require.Empty(t, page.Rows)
			page, err = service.Create(ctx, "owner", SnapshotRequest{Query: snapshotTestQuery(t, `{"sort":{"field":"document_date"}}`)})
			require.NoError(t, err)
			require.Len(t, page.Rows, 1)
			require.Empty(t, page.Rows[0].SortKey)
			require.NoError(t, service.Close())
		}
	}
}
