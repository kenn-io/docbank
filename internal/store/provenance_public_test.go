package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNodeProvenanceReturnsStableBoundedHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	ingest, err := s.BeginIngest(ctx, "cli", "/synthetic/import")
	require.NoError(t, err)
	node, added, err := s.IngestFile(ctx, ingest, s.RootID(), "report.txt",
		fakeHash("origin"), 6, "text/plain", "/synthetic/report.txt", "2026-01-02T03:04:05Z")
	require.NoError(t, err)
	require.True(t, added)

	var originalIdentity string
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT identity FROM provenance WHERE node_id=?`, node.ID,
	).Scan(&originalIdentity))
	successorIngest, err := newUUIDv4()
	require.NoError(t, err)
	successorMTime := "2026-07-01T12:00:00.000000000Z"
	successor := metadataProvenance{
		Type: metadataProvenanceType, NodeID: node.ID, IngestID: successorIngest,
		OriginalPath: "/corrected/report.txt", OriginalMTime: &successorMTime,
		Supersedes: &originalIdentity,
	}
	successor.Identity, err = provenanceIdentity(successor)
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ingests(id,started_at,source_kind,source_desc)
			VALUES(?,?,?,?)`, successorIngest, "2099-01-01T00:00:00.000000000Z",
			"watch", "agent-sessions"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO provenance(
			identity,node_id,ingest_id,original_path,original_mtime,supersedes)
			VALUES(?,?,?,?,?,?)`, successor.Identity, successor.NodeID, successor.IngestID,
			successor.OriginalPath, successor.OriginalMTime, successor.Supersedes)
		return err
	}))

	page, err := s.NodeProvenance(ctx, node.ID, 1, 0)
	require.NoError(t, err)
	assert.Equal(t, node.ID, page.Node.ID)
	assert.Equal(t, "/report.txt", page.Path)
	assert.Equal(t, 2, page.Total)
	assert.Equal(t, 1, page.Limit)
	require.Len(t, page.Items, 1)
	assert.Equal(t, successor.Identity, page.Items[0].Identity)
	assert.Equal(t, "watch", page.Items[0].SourceKind)
	assert.Equal(t, "agent-sessions", page.Items[0].SourceDescription)
	assert.True(t, page.Items[0].Active)
	assert.Equal(t, &originalIdentity, page.Items[0].Supersedes)

	older, err := s.NodeProvenance(ctx, node.ID, 1, 1)
	require.NoError(t, err)
	require.Len(t, older.Items, 1)
	assert.Equal(t, originalIdentity, older.Items[0].Identity)
	assert.False(t, older.Items[0].Active)

	trashed, _, err := s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)
	page, err = s.NodeProvenance(ctx, trashed.ID, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, page.Path)
	assert.NotNil(t, page.Node.TrashedAt)
}

func TestNodeProvenanceRejectsDirectoriesAndInvalidPages(t *testing.T) {
	s := newTestStore(t)
	_, err := s.NodeProvenance(t.Context(), s.RootID(), 10, 0)
	require.ErrorIs(t, err, ErrNotFile)
	_, err = s.NodeProvenance(t.Context(), 99999, 10, 0)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.NodeProvenance(t.Context(), s.RootID(), 0, 0)
	require.ErrorContains(t, err, "limit")
	_, err = s.NodeProvenance(t.Context(), s.RootID(), 10, -1)
	require.ErrorContains(t, err, "offset")
}
