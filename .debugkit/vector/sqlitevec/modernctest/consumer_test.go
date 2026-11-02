// Package modernctest exercises a consumer with no C SQLite driver linked.
package modernctest

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
	_ "modernc.org/sqlite"
	_ "modernc.org/sqlite/vec"
)

func TestModerncConsumer(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	ctx := t.Context()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "search.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(db.Close()) })
	_, err = db.ExecContext(ctx, `CREATE TABLE documents (id INTEGER PRIMARY KEY, body TEXT, revision INTEGER, embed_gen INTEGER);
        INSERT INTO documents (id, body, revision) VALUES (1, 'connection reuse', 1)`)
	require.NoError(err)
	store, err := sqlitevec.New[int64, int64](ctx, db, sqlitevec.Schema{
		DocsTable: "documents", IDColumn: "id", ContentColumn: "body",
		RevisionColumn: "revision", EmbedGenColumn: "embed_gen", VectorsPrefix: "vectors",
	})
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "fixture", Dimensions: 3}, sqlitevec.StateBuilding))
	require.NoError(store.SaveVectors(ctx, 1, 1, int64(1), []vector.ChunkVector{{ChunkIndex: 0, Vector: []float32{1, 0, 0}}}))
	hits, err := store.QueryGeneration(ctx, 1, []float32{1, 0, 0}, 10)
	require.NoError(err)
	require.Len(hits, 1)
	require.Equal(int64(1), hits[0].Doc)
	require.InDelta(1, hits[0].Score, 1e-6)
}
