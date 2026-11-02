package sqlitevec_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// TestQueryGenerationPlanScansKNNOnce pins QueryGeneration's join order:
// the knn CTE must be materialized and drive the join as the outermost
// loop, so the brute-force vec0 scan runs exactly once per query. When
// SQLite instead plans the CTE as a co-routine on the inner side of the
// join, the entire scan re-runs once per chunk-map row — observed as a
// >9-minute query over a 116k x 2560-dim generation whose single scan
// takes ~300ms.
func TestQueryGenerationPlanScansKNNOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setup(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body) VALUES (1, 'a cat sat'), (2, 'a dog ran')`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	_, err = vector.Fill(ctx, store, 1, topicEncoder())
	require.NoError(err)

	sqlText, args, err := store.QueryGenerationSQLForTest(1, []float32{1, 0, 0}, 10)
	require.NoError(err)

	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+sqlText, args...)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()

	var details []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		require.NoError(rows.Scan(&id, &parent, &notUsed, &detail))
		details = append(details, detail)
	}
	require.NoError(rows.Err())
	joined := strings.Join(details, "\n")

	assert.Contains(joined, "MATERIALIZE",
		"knn CTE must be materialized, not a re-runnable co-routine:\n%s", joined)

	knnScan := slices.IndexFunc(details, func(d string) bool {
		return strings.Contains(d, "SCAN knn")
	})
	chunkProbe := slices.IndexFunc(details, func(d string) bool {
		return strings.Contains(d, " c USING")
	})
	require.GreaterOrEqual(knnScan, 0, "expected a SCAN knn step:\n%s", joined)
	require.GreaterOrEqual(chunkProbe, 0, "expected an indexed chunk-map probe:\n%s", joined)
	assert.Less(knnScan, chunkProbe,
		"knn must be the outermost loop, probing the chunk map per hit:\n%s", joined)
}
