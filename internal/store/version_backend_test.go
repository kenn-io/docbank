package store

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

const (
	historyFirst   = "11111111-1111-4111-8111-111111111111"
	historySecond  = "22222222-2222-4222-8222-222222222222"
	historyCurrent = "33333333-3333-4333-8333-333333333333"
	historyOther   = "44444444-4444-4444-8444-444444444444"
	historyHash    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	historyMD5     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	historyTime    = "2026-01-01T00:00:00.000000000Z"
)

func TestContentVersionQueriesSQLite(t *testing.T) {
	t.Parallel()
	checkContentVersionQueries(t, newTestStore(t))
}

func TestContentVersionQueriesPostgres(t *testing.T) {
	t.Parallel()
	checkContentVersionQueries(t, newPostgresQueryStore(t))
}

// Seed only retained authority used by these queries, including a reverted
// head and another file sharing its bytes. Full mutations are not ported yet.
func seedContentHistory(t *testing.T, s *Store) (int64, int64) {
	t.Helper()
	ctx := t.Context()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	nodeID, err := insertNodeTx(ctx, tx, s.RootID(), "history.txt", nodeKindFile, new(historyCurrent), historyTime)
	require.NoError(t, err)
	otherID, err := insertNodeTx(ctx, tx, s.RootID(), "other.txt", nodeKindFile, new(historyOther), historyTime)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO blobs(hash,size,created_at) VALUES
		($1,17,$2), ($3,23,$2)`, historyHash, historyTime,
		"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO blob_checksums(blob_sha256,md5) VALUES ($1,$2)`, historyHash, historyMD5)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO content_versions
		(version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,
		 introduced_operation_id,transition_kind,source_version_id) VALUES
		('11111111-1111-4111-8111-111111111111',$1,$2,17,NULL,$3,1,
		 'aaaaaaaa-1111-4111-8111-111111111111','content_create',NULL),
		('22222222-2222-4222-8222-222222222222',$1,
		 'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc',23,'text/plain',$3,2,
		 'aaaaaaaa-2222-4222-8222-222222222222','content_replace',NULL),
		('33333333-3333-4333-8333-333333333333',$1,$2,17,NULL,$3,3,
		 'aaaaaaaa-3333-4333-8333-333333333333','content_revert','11111111-1111-4111-8111-111111111111'),
		('44444444-4444-4444-8444-444444444444',$4,$2,17,NULL,$3,1,
		 'aaaaaaaa-4444-4444-8444-444444444444','content_create',NULL)`, nodeID, historyHash, historyTime, otherID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE nodes SET revision=3 WHERE id=$1`, nodeID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return nodeID, otherID
}

func checkContentVersionQueries(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	nodeID, otherID := seedContentHistory(t, s)
	expected := ContentVersion{ID: historyCurrent, NodeID: nodeID, BlobHash: historyHash,
		MD5: historyMD5, Size: 17, RecordedAt: historyTime, NodeRevision: 3,
		IntroducedOperationID: "aaaaaaaa-3333-4333-8333-333333333333",
		TransitionKind:        "content_revert", SourceVersionID: new(historyFirst)}
	version, err := s.ContentVersionByID(ctx, historyCurrent)
	require.NoError(t, err)
	require.Equal(t, expected, version)
	view, err := s.ContentVersionViewByID(ctx, nodeID, historySecond)
	require.NoError(t, err)
	require.Equal(t, nodeID, view.Node.ID)
	require.Equal(t, historyCurrent, view.Node.CurrentVersionID)
	require.Equal(t, int64(3), view.Node.Revision)
	require.Equal(t, ContentVersion{ID: historySecond, NodeID: nodeID,
		BlobHash: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Size:     23, MimeType: "text/plain", RecordedAt: historyTime, NodeRevision: 2,
		IntroducedOperationID: "aaaaaaaa-2222-4222-8222-222222222222", TransitionKind: "content_replace"}, view.Version)
	_, err = s.ContentVersionViewByID(ctx, otherID, historySecond)
	require.ErrorIs(t, err, ErrNotFound)
	for _, id := range []string{"not-a-version", "55555555-5555-4555-8555-555555555555"} {
		_, err = s.ContentVersionByID(ctx, id)
		require.ErrorIs(t, err, ErrNotFound)
	}

	page, total, err := s.ContentVersions(ctx, nodeID, 2, 0)
	require.NoError(t, err)
	require.Equal(t, 3, total)
	require.Len(t, page, 2)
	require.Equal(t, expected, page[0])
	require.Equal(t, view.Version, page[1])
	page, total, err = s.ContentVersions(ctx, nodeID, 2, 2)
	require.NoError(t, err)
	require.Equal(t, 3, total)
	require.Len(t, page, 1)
	require.Equal(t, historyFirst, page[0].ID)
	page, total, err = s.ContentVersions(ctx, nodeID, 2, 3)
	require.NoError(t, err)
	require.Equal(t, 3, total)
	require.Empty(t, page)
	_, _, err = s.ContentVersions(ctx, s.RootID(), 2, 0)
	require.ErrorIs(t, err, ErrNotFile)
	_, _, err = s.ContentVersions(ctx, otherID+1000, 2, 0)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = s.writeDB.ExecContext(ctx, `UPDATE nodes SET trashed_at=$1 WHERE id=$2`, historyTime, nodeID)
	require.NoError(t, err)
	view, err = s.ContentVersionViewByID(ctx, nodeID, historyFirst)
	require.NoError(t, err)
	require.Equal(t, new(historyTime), view.Node.TrashedAt)
	require.Equal(t, historyFirst, view.Version.ID)
}

func TestContentVersionSnapshotPostgres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newPostgresQueryStore(t)
	db := s.db
	nodeID, _ := seedContentHistory(t, s)

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	var config *pgx.ConnConfig
	err = conn.Raw(func(driverConn any) error {
		pgConn, ok := driverConn.(*stdlib.Conn)
		require.True(t, ok)
		config = pgConn.Conn().Config().Copy()
		return nil
	})
	require.NoError(t, conn.Close())
	require.NoError(t, err)
	var pruneErr error
	config.Tracer = &pruneAfterNodeRead{prune: func() {
		_, pruneErr = db.ExecContext(ctx, `DELETE FROM content_versions WHERE version_id=$1`, historySecond)
	}}
	readDB := stdlib.OpenDB(*config)
	t.Cleanup(func() { require.NoError(t, readDB.Close()) })
	reader := &Store{db: readDB}
	view, err := reader.ContentVersionViewByID(ctx, nodeID, historySecond)
	require.NoError(t, pruneErr)
	require.NoError(t, err)
	require.Equal(t, historyCurrent, view.Node.CurrentVersionID)
	require.Equal(t, historySecond, view.Version.ID)
	_, err = s.ContentVersionViewByID(ctx, nodeID, historySecond)
	require.ErrorIs(t, err, ErrNotFound, "a later request observes the committed prune")
}

// The trace orders a real concurrent commit between the view's two SELECTs.
type pruneAfterNodeRead struct {
	once  sync.Once
	prune func()
}

func (*pruneAfterNodeRead) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (trace *pruneAfterNodeRead) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err == nil && data.CommandTag.Select() {
		trace.once.Do(trace.prune)
	}
}
