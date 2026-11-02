package sqlitevec_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVec0LoadsHermetically confirms the sqlite-vec extension is available
// through the modernc SQLite driver, so the backend's tests need no external setup.
func TestVec0LoadsHermetically(t *testing.T) {
	require := require.New(t)

	db, err := openSQLiteTestDB(t, ":memory:")
	require.NoError(err)
	t.Cleanup(func() { require.NoError(db.Close()) })

	_, err = db.ExecContext(t.Context(), `CREATE VIRTUAL TABLE v USING vec0(embedding float[3])`)
	require.NoError(err)

	_, err = db.ExecContext(t.Context(), `INSERT INTO v(rowid, embedding) VALUES (1, vec_f32(?))`, `[1,2,3]`)
	require.NoError(err)

	var rowid int64
	var distance float64
	err = db.QueryRowContext(t.Context(),
		`SELECT rowid, distance FROM v WHERE embedding MATCH vec_f32(?) ORDER BY distance LIMIT 1`,
		`[1,2,3]`,
	).Scan(&rowid, &distance)
	require.NoError(err)

	require.Equal(int64(1), rowid)
	require.InDelta(0, distance, 1e-6, "identical vectors have zero distance")
}
