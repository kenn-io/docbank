package sqlitevec_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

func fillGeneration(t *testing.T, store *sqlitevec.Store[int64, int64], gen int64) {
	t.Helper()
	_, err := vector.Fill(t.Context(), store, gen, topicEncoder())
	require.NoError(t, err)
}

func generationByKey(t *testing.T, store *sqlitevec.Store[int64, int64], key int64) sqlitevec.GenerationInfo[int64] {
	t.Helper()
	gens, err := store.Generations(t.Context())
	require.NoError(t, err)
	for _, gen := range gens {
		if gen.Key == key {
			return gen
		}
	}
	require.FailNowf(t, "missing generation", "key %d", key)
	return sqlitevec.GenerationInfo[int64]{}
}

func TestCoverageSeparatesVectorsStampsAndStaleDocuments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setupWithRevision(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES
		(1, 'a cat sat', 1),
		(2, '   ', 1),
		(3, 'a dog ran', 1)`)
	require.NoError(err)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateActive))
	fillGeneration(t, store, 1)

	_, err = db.ExecContext(ctx, `UPDATE messages SET body = 'a dog jumped', last_modified = 2 WHERE id = 3`)
	require.NoError(err)

	coverage, err := store.Coverage(ctx, 1, "")
	require.NoError(err)
	assert.Equal(sqlitevec.Coverage{Embedded: 1, Skipped: 1, Backlog: 1}, coverage)

	filtered, err := store.Coverage(ctx, 1, "d.id = ?", int64(1))
	require.NoError(err)
	assert.Equal(sqlitevec.Coverage{Embedded: 1}, filtered)

	_, err = store.Coverage(ctx, 9, "")
	require.ErrorIs(err, sqlitevec.ErrGenerationNotFound)
}

func TestActivateRefusesAGenerationWithAStaleDocument(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setupWithRevision(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1), (2, 'a dog ran', 1)`)
	require.NoError(err)
	model := vector.Generation{Model: "m", Dimensions: 3}
	require.NoError(store.EnsureGeneration(ctx, 1, model, sqlitevec.StateActive))
	require.NoError(store.EnsureGeneration(ctx, 2, model, sqlitevec.StateBuilding))
	fillGeneration(t, store, 1)
	fillGeneration(t, store, 2)

	_, err = db.ExecContext(ctx, `UPDATE messages SET last_modified = 2 WHERE id = 2`)
	require.NoError(err)

	err = store.Activate(ctx, 2)
	require.ErrorIs(err, sqlitevec.ErrUncovered)
	var uncovered *sqlitevec.UncoveredError
	require.ErrorAs(err, &uncovered)
	assert.Equal(int64(1), uncovered.Backlog)
	assert.Equal(sqlitevec.StateActive, generationByKey(t, store, 1).State)
	assert.Equal(sqlitevec.StateBuilding, generationByKey(t, store, 2).State)

	live, err := store.LiveGenerations(ctx)
	require.NoError(err)
	assert.Equal([]int64{2, 1}, live, "a refused publication leaves the building generation searchable")
}

func TestActivatePublishesACoveredGenerationAndRetiresOtherLiveOnes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setupWithRevision(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1), (2, 'a dog ran', 1)`)
	require.NoError(err)
	model := vector.Generation{Model: "m", Dimensions: 3}
	require.NoError(store.EnsureGeneration(ctx, 1, model, sqlitevec.StateActive))
	require.NoError(store.EnsureGeneration(ctx, 2, vector.Generation{Model: "n", Dimensions: 3}, sqlitevec.StateBuilding))
	require.NoError(store.EnsureGeneration(ctx, 3, model, sqlitevec.StateBuilding))
	fillGeneration(t, store, 1)
	fillGeneration(t, store, 2)

	live, err := store.LiveGenerations(ctx)
	require.NoError(err)
	assert.Equal([]int64{2, 3, 1}, live, "building generations stay ahead of the active generation")

	require.NoError(store.Activate(ctx, 2))
	assert.Equal(sqlitevec.StateRetired, generationByKey(t, store, 1).State)
	assert.Equal(sqlitevec.StateActive, generationByKey(t, store, 2).State)
	assert.Equal(sqlitevec.StateRetired, generationByKey(t, store, 3).State)

	live, err = store.LiveGenerations(ctx)
	require.NoError(err)
	assert.Equal([]int64{2}, live)

	hits, err := store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 10)
	require.NoError(err)
	require.NotEmpty(hits)
	assert.Equal(int64(1), hits[0].Doc, "retiring a generation keeps its vectors until reclamation")

	active, ok, err := store.ActiveGeneration(ctx)
	require.NoError(err)
	require.True(ok)
	assert.Equal(int64(2), active.Key)
	assert.Equal(sqlitevec.StateActive, active.State)
	assert.Equal(model.Fingerprint(), generationByKey(t, store, 1).Fingerprint)
	assert.Equal(vector.Generation{Model: "n", Dimensions: 3}.Fingerprint(), active.Fingerprint)
}

func TestActivateRefusesAReclaimedGeneration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setupWithRevision(t)
	model := vector.Generation{Model: "m", Dimensions: 3}
	require.NoError(store.EnsureGeneration(ctx, 1, model, sqlitevec.StateBuilding))
	require.NoError(store.EnsureGeneration(ctx, 2, model, sqlitevec.StateBuilding))
	require.NoError(store.Activate(ctx, 2))
	require.NoError(store.Reclaim(ctx, 1))

	err := store.Activate(ctx, 1)
	require.ErrorIs(err, sqlitevec.ErrRetired)
	require.NotErrorIs(err, sqlitevec.ErrUncovered)
	assert.Equal(sqlitevec.StateRetired, generationByKey(t, store, 1).State)
	assert.Equal(sqlitevec.StateActive, generationByKey(t, store, 2).State)

	active, ok, err := store.ActiveGeneration(ctx)
	require.NoError(err)
	require.True(ok)
	assert.Equal(int64(2), active.Key)

	var vecTable string
	err = db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE name = 'message_vectors_v1'`).Scan(&vecTable)
	require.ErrorIs(err, sql.ErrNoRows)
}

func TestOlderStateMethodsCannotReviveAReclaimedGeneration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setupWithRevision(t)
	model := vector.Generation{Model: "m", Dimensions: 3}
	require.NoError(store.EnsureGeneration(ctx, 1, model, sqlitevec.StateBuilding))
	require.NoError(store.EnsureGeneration(ctx, 2, model, sqlitevec.StateBuilding))
	require.NoError(store.Activate(ctx, 2))
	require.NoError(store.Reclaim(ctx, 1))

	require.ErrorIs(store.SetGenerationState(ctx, 1, sqlitevec.StateActive), sqlitevec.ErrRetired)
	require.ErrorIs(store.EnsureGeneration(ctx, 1, model, sqlitevec.StateBuilding), sqlitevec.ErrRetired)
	require.NoError(store.EnsureGeneration(ctx, 1, model, sqlitevec.StateRetired))
	require.NoError(store.SetGenerationState(ctx, 1, sqlitevec.StateRetired))
	assert.Equal(sqlitevec.StateRetired, generationByKey(t, store, 1).State)

	live, err := store.LiveGenerations(ctx)
	require.NoError(err)
	assert.Equal([]int64{2}, live, "search never reaches the dropped table")
	var vecTable string
	err = db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE name = 'message_vectors_v1'`).Scan(&vecTable)
	require.ErrorIs(err, sql.ErrNoRows, "ensuring a retired generation does not recreate its storage")
}

func TestActivateRefusesARetiredGeneration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	_, store := setupWithRevision(t)
	model := vector.Generation{Model: "m", Dimensions: 3}
	require.NoError(store.EnsureGeneration(ctx, 1, model, sqlitevec.StateActive))
	require.NoError(store.EnsureGeneration(ctx, 2, model, sqlitevec.StateBuilding))
	fillGeneration(t, store, 1)
	fillGeneration(t, store, 2)
	require.NoError(store.Activate(ctx, 2))

	err := store.Activate(ctx, 1)
	require.ErrorIs(err, sqlitevec.ErrRetired)
	assert.Equal(sqlitevec.StateRetired, generationByKey(t, store, 1).State)
	assert.Equal(sqlitevec.StateActive, generationByKey(t, store, 2).State)
}

func TestActivateAllowsAnEmptyCorpus(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	_, store := setupWithRevision(t)
	require.NoError(store.EnsureGeneration(ctx, 1, vector.Generation{Model: "m", Dimensions: 3}, sqlitevec.StateBuilding))

	coverage, err := store.Coverage(ctx, 1, "")
	require.NoError(err)
	require.Equal(sqlitevec.Coverage{}, coverage)
	require.NoError(store.Activate(ctx, 1))
	require.Equal(sqlitevec.StateActive, generationByKey(t, store, 1).State)
}

func TestActiveGenerationReturnsTheNewestActiveGeneration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	_, store := setup(t)

	active, ok, err := store.ActiveGeneration(ctx)
	require.NoError(err)
	assert.False(ok)
	assert.Zero(active.Key)

	model := vector.Generation{Model: "m", Dimensions: 3}
	require.NoError(store.EnsureGeneration(ctx, 1, model, sqlitevec.StateActive))
	require.NoError(store.EnsureGeneration(ctx, 2, model, sqlitevec.StateActive))

	active, ok, err = store.ActiveGeneration(ctx)
	require.NoError(err)
	require.True(ok)
	assert.Equal(int64(2), active.Key)
}

func TestReclaimDropsRetiredStorageAndLeavesTheGenerationRow(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	db, store := setupWithRevision(t)

	_, err := db.ExecContext(ctx, `INSERT INTO messages (id, body, last_modified) VALUES (1, 'a cat sat', 1), (2, 'a dog ran', 1)`)
	require.NoError(err)
	model := vector.Generation{Model: "m", Dimensions: 3}
	require.NoError(store.EnsureGeneration(ctx, 1, model, sqlitevec.StateActive))
	require.NoError(store.EnsureGeneration(ctx, 2, model, sqlitevec.StateBuilding))
	fillGeneration(t, store, 1)
	fillGeneration(t, store, 2)

	err = store.Reclaim(ctx, 2)
	require.ErrorContains(err, "retired")
	hits, err := store.QueryGeneration(ctx, 2, vector.Vector{1, 0, 0}, 10)
	require.NoError(err)
	assert.NotEmpty(hits, "refusing reclamation leaves the building generation searchable")

	require.NoError(store.Activate(ctx, 2))
	require.NoError(store.Reclaim(ctx, 1))
	require.NoError(store.Reclaim(ctx, 1), "reclaiming an already reclaimed generation is safe")

	assert.Equal(sqlitevec.StateRetired, generationByKey(t, store, 1).State)
	_, err = store.QueryGeneration(ctx, 1, vector.Vector{1, 0, 0}, 10)
	require.Error(err)

	hits, err = store.QueryGeneration(ctx, 2, vector.Vector{0, 1, 0}, 10)
	require.NoError(err)
	require.NotEmpty(hits)
	assert.Equal(int64(2), hits[0].Doc)

	var chunks int
	require.NoError(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_vectors_chunks`).Scan(&chunks))
	assert.Equal(2, chunks, "reclamation deletes the retired generation's chunk rows and keeps the published generation")
	var stamps int
	require.NoError(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_vectors_stamps WHERE ordinal = 1`).Scan(&stamps))
	assert.Zero(stamps)

	var vecTable string
	err = db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE name = 'message_vectors_v1'`).Scan(&vecTable)
	require.ErrorIs(err, sql.ErrNoRows)

	err = store.Reclaim(ctx, 9)
	require.ErrorIs(err, sqlitevec.ErrGenerationNotFound)
}

func TestActivateReportsAMissingGeneration(t *testing.T) {
	require := require.New(t)
	_, store := setup(t)
	err := store.Activate(t.Context(), 4)
	require.ErrorIs(err, sqlitevec.ErrGenerationNotFound)
	require.NotErrorIs(err, sqlitevec.ErrUncovered)
}
