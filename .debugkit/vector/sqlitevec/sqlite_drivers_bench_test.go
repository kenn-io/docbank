//go:build !windows && cgo

package sqlitevec_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	cgosqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
	_ "modernc.org/sqlite/vec"

	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

type sqliteDriverBench struct {
	name       string
	driverName string
	setup      func()
}

var sqliteDriverBenches = []sqliteDriverBench{
	{name: "modernc", driverName: "sqlite"},
	{name: "mattn", driverName: "sqlite3", setup: cgosqlitevec.Auto},
}

func BenchmarkSQLiteDriverQueryGeneration(b *testing.B) {
	for _, driver := range sqliteDriverBenches {
		b.Run(driver.name, func(b *testing.B) {
			require := require.New(b)
			ctx := b.Context()
			_, store := setupBenchmarkStore(b, driver, 1000, 16)
			query := benchVector(0, 16)

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				hits, err := store.QueryGeneration(ctx, int64(1), query, 10)
				if err != nil {
					b.StopTimer()
					require.NoError(err)
				}
				if len(hits) == 0 {
					b.StopTimer()
					require.NotEmpty(hits)
				}
			}
		})
	}
}

func BenchmarkSQLiteDriverSaveVectors(b *testing.B) {
	for _, driver := range sqliteDriverBenches {
		b.Run(driver.name, func(b *testing.B) {
			require := require.New(b)
			ctx := b.Context()
			documents := 1000
			_, store := setupBenchmarkStore(b, driver, documents, 16)

			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				doc := int64(i%documents + 1)
				err := store.SaveVectors(ctx, int64(1), doc, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: benchVector(i, 16)}})
				if err != nil {
					b.StopTimer()
					require.NoError(err)
				}
			}
		})
	}
}

func setupBenchmarkStore(b *testing.B, driver sqliteDriverBench, documents, dimensions int) (*sql.DB, *sqlitevec.Store[int64, int64]) {
	b.Helper()
	require := require.New(b)
	ctx := b.Context()
	if driver.setup != nil {
		driver.setup()
	}

	db, err := sql.Open(driver.driverName, filepath.Join(b.TempDir(), "bench.db"))
	require.NoError(err)
	b.Cleanup(func() { require.NoError(db.Close()) })
	db.SetMaxOpenConns(1)

	_, err = db.ExecContext(ctx, `CREATE TABLE messages (id INTEGER PRIMARY KEY, body TEXT, embed_gen INTEGER)`)
	require.NoError(err)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(err)
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO messages (id, body) VALUES (?, ?)`)
	require.NoError(err)
	defer stmt.Close()
	for i := 1; i <= documents; i++ {
		_, err = stmt.ExecContext(ctx, i, fmt.Sprintf("document %d", i))
		require.NoError(err)
	}
	require.NoError(stmt.Close())
	require.NoError(tx.Commit())

	store, err := sqlitevec.New[int64, int64](ctx, db, sqlitevec.Schema{
		DocsTable:      "messages",
		IDColumn:       "id",
		ContentColumn:  "body",
		EmbedGenColumn: "embed_gen",
		VectorsPrefix:  "message_vectors",
	})
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, int64(1), vector.Generation{Model: "bench", Dimensions: dimensions}, sqlitevec.StateActive))

	for i := 1; i <= documents; i++ {
		err = store.SaveVectors(ctx, int64(1), int64(i), nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: benchVector(i, dimensions)}})
		require.NoError(err)
	}
	return db, store
}

func benchVector(seed, dimensions int) vector.Vector {
	out := make(vector.Vector, dimensions)
	for i := range out {
		out[i] = float32((seed*(i+3))%17 + 1)
	}
	return out
}
