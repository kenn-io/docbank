package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIndexProjectionStateAdvancesOnlyWhenAuthorityChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docbank.db")
	s, err := Open(path)
	require.NoError(t, err)
	ctx := t.Context()

	first, err := s.ObserveIndexProjection(
		ctx, IndexProjectionLexical, "", fakeHash("11"), LocalIndexAuthorizationChecksum(),
	)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), first.SourceWatermark)
	assert.Equal(t, uint64(1), first.AuthorizationWatermark)

	replayed, err := s.ObserveIndexProjection(
		ctx, IndexProjectionLexical, "", fakeHash("11"), LocalIndexAuthorizationChecksum(),
	)
	require.NoError(t, err)
	assert.Equal(t, first, replayed)

	changed, err := s.ObserveIndexProjection(
		ctx, IndexProjectionLexical, "", fakeHash("12"), LocalIndexAuthorizationChecksum(),
	)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), changed.SourceWatermark)
	assert.Equal(t, uint64(1), changed.AuthorizationWatermark)

	require.NoError(t, s.Close())
	restartedStore, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restartedStore.Close()) })
	restarted, err := restartedStore.IndexProjectionState(ctx, IndexProjectionLexical, "")
	require.NoError(t, err)
	assert.Equal(t, changed, restarted)
}

func TestLexicalIndexRepairStagesUnreachableThenRetainsRollback(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	seedPublishedRendition(t, s, "first source")

	before, err := s.CaptureLexicalIndexSource(ctx)
	require.NoError(t, err)
	candidate, err := s.StageLexicalIndexGeneration(ctx, before)
	require.NoError(t, err)
	_, err = s.ActiveLexicalGeneration(ctx)
	require.ErrorIs(t, err, ErrNotFound, "staging must not expose a candidate")
	require.NoError(t, s.ValidateLexicalIndexGeneration(ctx, candidate))

	require.NoError(t, s.PublishLexicalIndexGeneration(
		ctx, candidate, time.Now().UTC(), time.Hour,
	))
	active, err := s.ActiveLexicalGeneration(ctx)
	require.NoError(t, err)
	assert.Equal(t, candidate.Generation.ID, active.ID)
	state, err := s.IndexProjectionState(ctx, IndexProjectionLexical, "")
	require.NoError(t, err)
	assert.Equal(t, candidate.Generation.ID, state.GenerationID)
	assert.Equal(t, state.SourceWatermark, state.IndexWatermark)

	seedPublishedRendition(t, s, "second source")
	nextSource, err := s.CaptureLexicalIndexSource(ctx)
	require.NoError(t, err)
	next, err := s.StageLexicalIndexGeneration(ctx, nextSource)
	require.NoError(t, err)
	require.NoError(t, s.PublishLexicalIndexGeneration(
		ctx, next, time.Now().UTC(), time.Hour,
	))

	var retained int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM current_rendition_roots
		WHERE target_kind='lexical_generation' AND target_id=? AND active=1`,
		candidate.Generation.ID).Scan(&retained))
	assert.Equal(t, 1, retained, "the replaced healthy generation remains rollback-eligible")
}

func TestLexicalIndexRepairRejectsChangedSourceBeforeCutover(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	seedPublishedRendition(t, s, "first source")
	source, err := s.CaptureLexicalIndexSource(ctx)
	require.NoError(t, err)
	candidate, err := s.StageLexicalIndexGeneration(ctx, source)
	require.NoError(t, err)

	seedPublishedRendition(t, s, "changed source")
	err = s.PublishLexicalIndexGeneration(ctx, candidate, time.Now().UTC(), time.Hour)
	require.ErrorIs(t, err, ErrLexicalGenerationStale)
	_, err = s.ActiveLexicalGeneration(ctx)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, s.DiscardLexicalIndexGeneration(ctx, candidate.Generation.ID))
}

func TestLexicalIndexRepairReplacesCorruptServingManifest(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	seedPublishedRendition(t, s, "repairable source")
	source, err := s.CaptureLexicalIndexSource(ctx)
	require.NoError(t, err)
	first, err := s.StageLexicalIndexGeneration(ctx, source)
	require.NoError(t, err)
	require.NoError(t, s.PublishLexicalIndexGeneration(
		ctx, first, time.Now().UTC(), time.Hour,
	))
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE rendition_lexical_generation_manifests
			SET manifest_digest=? WHERE generation_id=?`, fakeHash("bad"), first.Generation.ID)
		return err
	}))

	freshSource, err := s.CaptureLexicalIndexSource(ctx)
	require.NoError(t, err)
	replacement, err := s.StageLexicalIndexGeneration(ctx, freshSource)
	require.NoError(t, err)
	require.NotEqual(t, first.Generation.ID, replacement.Generation.ID)
	require.NoError(t, s.ValidateLexicalIndexGeneration(ctx, replacement))
	require.NoError(t, s.PublishLexicalIndexGeneration(
		ctx, replacement, time.Now().UTC(), time.Hour,
	))
	active, err := s.ActiveLexicalGeneration(ctx)
	require.NoError(t, err)
	assert.Equal(t, replacement.Generation.ID, active.ID)
}

func seedPublishedRendition(t *testing.T, s *Store, body string) {
	t.Helper()
	buildID := testSHA256([]byte("index-build:" + body))
	sourceHash := testSHA256([]byte("index-source:" + body))
	_, err := s.CreateFile(
		t.Context(), s.RootID(), "synthetic-"+buildID[:8]+".pdf",
		sourceHash, int64(len(body)), "application/pdf",
	)
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		for _, hash := range []string{catalogEvidenceBlobHash, catalogMarkdownBlobHash} {
			if err := s.EnsureBlobTx(tx, hash, int64(len(catalogBlobContents[hash]))); err != nil {
				return err
			}
		}
		return nil
	}))
	profile := catalogProcessingProfile(t, false)
	build := lexicalSearchBuild(s, profile, buildID, body)
	build.SourceSHA256 = sourceHash
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
}
