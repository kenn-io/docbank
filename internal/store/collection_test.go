package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectionsExposeActiveOperationalMembership(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	first, err := s.BeginIngest(ctx, "cli", "First synthetic import")
	require.NoError(t, err)
	firstNode, added, err := s.IngestFile(ctx, first, s.RootID(), "b.txt", fakeHash("a1"),
		4, "text/plain", "/synthetic/b.txt", "")
	require.NoError(t, err)
	require.True(t, added)
	secondNode, added, err := s.IngestFile(ctx, first, s.RootID(), "a.txt", fakeHash("b2"),
		6, "text/plain", "/synthetic/a.txt", "")
	require.NoError(t, err)
	require.True(t, added)

	second, err := s.BeginIngest(ctx, "watch", "Second synthetic import")
	require.NoError(t, err)
	thirdNode, err := s.IngestFileExact(ctx, second, s.RootID(), "c.txt", fakeHash("c3"),
		8, "text/plain", "c.txt", "")
	require.NoError(t, err)

	// One node may belong to multiple active operational runs, while repeated
	// facts within a run must not inflate its current counts.
	addCollectionMembership(t, s, second, firstNode.ID, "/other/b.txt", nil)
	addCollectionMembership(t, s, first, firstNode.ID, "/duplicate/b.txt", nil)

	embedded, err := s.BeginCallerSuppliedIngest(ctx, "cli", "Application assertion")
	require.NoError(t, err)
	_, err = s.IngestFileExact(ctx, embedded, s.RootID(), "private.txt", fakeHash("d4"),
		10, "text/plain", "opaque/private.txt", "")
	require.NoError(t, err)

	collections, total, err := s.Collections(ctx, 100, 0)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, collections, 2)
	assert.Equal(t, second.ID(), collections[0].ID)
	assert.Equal(t, int64(2), collections[0].FileCount)
	assert.Equal(t, int64(12), collections[0].TotalBytes)
	assert.Equal(t, first.ID(), collections[1].ID)
	assert.Equal(t, int64(2), collections[1].FileCount)
	assert.Equal(t, int64(10), collections[1].TotalBytes)
	assert.Nil(t, collections[1].Label)
	assert.Equal(t, int64(1), collections[1].LabelRevision)
	assert.Equal(t, first.record.StartedAt, collections[1].LabelUpdatedAt)

	page, err := s.CollectionMembers(ctx, first.ID(), 1, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, page.Total)
	require.Len(t, page.Items, 1)
	assert.Equal(t, secondNode.ID, page.Items[0].Node.ID)
	assert.Equal(t, "/a.txt", page.Items[0].Path)
	assert.Equal(t, collections[1], page.Collection)

	page, err = s.CollectionMembers(ctx, first.ID(), 1, 1)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, firstNode.ID, page.Items[0].Node.ID)
	assert.Equal(t, "/b.txt", page.Items[0].Path)

	detail, err := s.CollectionByID(ctx, second.ID())
	require.NoError(t, err)
	assert.Equal(t, thirdNode.Size+firstNode.Size, detail.TotalBytes)
	_, err = s.CollectionByID(ctx, embedded.ID())
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.CollectionMembers(ctx, embedded.ID(), 10, 0)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestCollectionMembershipFollowsSupersessionAndTrash(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	dir, err := s.Mkdir(ctx, s.RootID(), "records")
	require.NoError(t, err)
	run, err := s.BeginIngest(ctx, "cli", "Synthetic import")
	require.NoError(t, err)
	node, added, err := s.IngestFile(ctx, run, dir.ID, "note.txt", fakeHash("a1"),
		4, "text/plain", "/synthetic/note.txt", "")
	require.NoError(t, err)
	require.True(t, added)

	collection, err := s.CollectionByID(ctx, run.ID())
	require.NoError(t, err)
	assert.Equal(t, int64(1), collection.FileCount)

	_, _, err = s.Trash(ctx, dir.ID, UnconditionalRev)
	require.NoError(t, err)
	_, err = s.CollectionByID(ctx, run.ID())
	require.ErrorIs(t, err, ErrNotFound)
	collections, total, err := s.Collections(ctx, 100, 0)
	require.NoError(t, err)
	assert.Empty(t, collections)
	assert.Zero(t, total)

	_, _, err = s.Restore(ctx, dir.ID, UnconditionalRev)
	require.NoError(t, err)
	collection, err = s.CollectionByID(ctx, run.ID())
	require.NoError(t, err)
	assert.Equal(t, int64(1), collection.FileCount)

	var prior string
	require.NoError(t, s.db.QueryRow(`SELECT identity FROM provenance WHERE node_id=?`, node.ID).Scan(&prior))
	correction, err := s.BeginCallerSuppliedIngest(ctx, "correction", "Synthetic correction")
	require.NoError(t, err)
	addCollectionMembership(t, s, correction, node.ID, "opaque/corrected.txt", &prior)
	_, err = s.CollectionByID(ctx, run.ID())
	require.ErrorIs(t, err, ErrNotFound)
}

func TestCollectionReadBounds(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "cli", "Synthetic import")
	require.NoError(t, err)
	_, _, err = s.IngestFile(ctx, run, s.RootID(), "note.txt", fakeHash("a1"),
		4, "text/plain", "/synthetic/note.txt", "")
	require.NoError(t, err)

	collections, total, err := s.Collections(ctx, 100, 0)
	require.NoError(t, err)
	assert.Len(t, collections, 1)
	assert.Equal(t, 1, total)
	_, _, err = s.Collections(ctx, 0, 0)
	require.Error(t, err)
	_, _, err = s.Collections(ctx, 1001, 0)
	require.Error(t, err)
	_, _, err = s.Collections(ctx, 10, -1)
	require.Error(t, err)
	_, err = s.CollectionMembers(ctx, run.ID(), 1001, 0)
	require.Error(t, err)
	_, err = s.CollectionMembers(ctx, run.ID(), 10, -1)
	require.Error(t, err)
}

func addCollectionMembership(
	t *testing.T, s *Store, run IngestRun, nodeID int64, originalPath string, supersedes *string,
) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := ensureIngestRunTx(ctx, tx, run); err != nil {
			return err
		}
		fact := metadataProvenance{
			Type: metadataProvenanceType, NodeID: nodeID, IngestID: run.ID(),
			OriginalPath: originalPath, Supersedes: supersedes,
		}
		identity, err := provenanceIdentity(fact)
		if err != nil {
			return err
		}
		fact.Identity = identity
		_, err = tx.Exec(`INSERT INTO provenance(
			identity,node_id,ingest_id,original_path,original_mtime,supersedes
		) VALUES(?,?,?,?,?,?)`, fact.Identity, fact.NodeID, fact.IngestID,
			fact.OriginalPath, fact.OriginalMTime, fact.Supersedes)
		return err
	}))
}
