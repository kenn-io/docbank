package sqlitevec_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// setupExport builds a revision-tracked store whose documents table carries
// a caller column (topic) beyond what the store itself reads.
func setupExport(t *testing.T) (*sql.DB, *sqlitevec.Store[int64, int64]) {
	t.Helper()
	db, err := openSQLiteTestDB(t, filepath.Join(t.TempDir(), "vec.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	_, err = db.ExecContext(t.Context(), `CREATE TABLE messages (
		id INTEGER PRIMARY KEY, body TEXT, topic TEXT, embed_gen INTEGER,
		last_modified INTEGER NOT NULL DEFAULT 0)`)
	require.NoError(t, err)

	store, err := sqlitevec.New[int64, int64](t.Context(), db, sqlitevec.Schema{
		DocsTable:      "messages",
		IDColumn:       "id",
		ContentColumn:  "body",
		EmbedGenColumn: "embed_gen",
		RevisionColumn: "last_modified",
		VectorsPrefix:  "message_vectors",
	})
	require.NoError(t, err)
	return db, store
}

// coveredIDs drains CoveredDocs into the document keys it returned, checking
// that every extra column scans as a string.
func coveredIDs(t *testing.T, snap *sqlitevec.Snapshot[int64, int64], q sqlitevec.DocQuery) []int64 {
	t.Helper()
	rows, err := snap.CoveredDocs(t.Context(), q)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var ids []int64
	for rows.Next() {
		var id int64
		dest := []any{&id}
		extra := make([]string, len(q.Columns))
		for i := range extra {
			dest = append(dest, &extra[i])
		}
		require.NoError(t, rows.Scan(dest...))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func TestGenerationsListsEveryState(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	_, store := setupExport(t)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	require.NoError(store.EnsureGeneration(ctx, 2, vector.Generation{Model: "n", Dimensions: 3}, sqlitevec.StateRetired))

	gens, err := store.Generations(ctx)
	require.NoError(err)
	require.Len(gens, 2)
	assert.Equal(t, sqlitevec.GenerationInfo[int64]{
		Key: 1, Fingerprint: vector.Generation{Model: "m", Dimensions: 3}.Fingerprint(),
		Dimension: 3, State: sqlitevec.StateActive,
	}, gens[0])
	assert.Equal(t, int64(2), gens[1].Key)
	assert.Equal(t, sqlitevec.StateRetired, gens[1].State)
}

func TestSnapshotExportsCoveredDocumentsAndChunks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setupExport(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, topic) VALUES
		(1, 'a cat sat', 'pets'), (2, 'a dog ran', 'pets'), (3, 'a bird flew', 'wild')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(err)

	// Document 2 changes after its fill: its stamp no longer matches, so it
	// is uncovered until the next fill.
	_, err = db.ExecContext(ctx, `UPDATE messages SET body = 'a dog sat', last_modified = 1 WHERE id = 2`)
	require.NoError(err)

	snap, err := store.Snapshot(ctx, 1)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(snap.Close()) })
	assert.Equal(int64(1), snap.Generation().Key)
	assert.Equal(3, snap.Generation().Dimension)
	assert.Equal(sqlitevec.StateActive, snap.Generation().State)

	covered := coveredIDs(t, snap, sqlitevec.DocQuery{
		Columns: []string{"topic"},
		Where:   "d.topic = ?",
		Args:    []any{"pets"},
	})
	assert.Equal([]int64{1}, covered, "the edited document is not covered")
	assert.Equal([]int64{1, 3}, coveredIDs(t, snap, sqlitevec.DocQuery{OrderBy: []string{"topic", "id"}}))

	uncovered, err := snap.UncoveredCount(ctx, "")
	require.NoError(err)
	assert.Equal(int64(1), uncovered)
	uncoveredWild, err := snap.UncoveredCount(ctx, "d.topic = ?", "wild")
	require.NoError(err)
	assert.Zero(uncoveredWild)

	chunks, err := snap.Chunks(ctx, 1)
	require.NoError(err)
	require.Len(chunks, 1)
	assert.Equal(0, chunks[0].ChunkIndex)
	assert.Equal(vector.Vector{1, 0, 0}, chunks[0].Vector)

	none, err := snap.Chunks(ctx, 99)
	require.NoError(err)
	assert.Empty(none)
}

func TestSnapshotRejectsBadIdentifiersAndUseAfterClose(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	_, store := setupExport(t)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))

	_, err := store.Snapshot(ctx, 7)
	require.ErrorIs(err, sqlitevec.ErrGenerationNotFound)

	snap, err := store.Snapshot(ctx, 1)
	require.NoError(err)
	rows, err := snap.CoveredDocs(ctx, sqlitevec.DocQuery{Columns: []string{"topic; DROP TABLE messages"}}) //nolint:kennlint // rowserrcheck: a validation error returns nil rows, nothing to check or close
	require.ErrorContains(err, "invalid identifier")
	require.Nil(rows)
	rows, err = snap.CoveredDocs(ctx, sqlitevec.DocQuery{OrderBy: []string{"id DESC"}}) //nolint:kennlint // rowserrcheck: same: nil rows on a validation error
	require.ErrorContains(err, "invalid identifier")
	require.Nil(rows)

	require.NoError(snap.Close())
	require.NoError(snap.Close())
	_, err = snap.Chunks(ctx, 1)
	require.ErrorIs(err, sql.ErrTxDone)
	_, err = snap.UncoveredCount(ctx, "")
	require.ErrorIs(err, sql.ErrTxDone)
}
