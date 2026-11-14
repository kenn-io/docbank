package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkPhotoQualityLifecycle(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	first := browsePhotoNode(t, s, "quality.jpg", browseHash("quality-lifecycle"), "image/jpeg")
	second := browsePhotoNode(t, s, "unavailable.jpg", browseHash("quality-unavailable"), "image/jpeg")
	recipe, err := document.BuiltInVisualPreviewRecipe("grid")
	require.NoError(t, err)
	for _, node := range []Node{first, second} {
		_, err := s.PublishVisualPreview(ctx, node.CurrentVersionID, readyVisualPreviewWithRecipe(t, node.BlobHash, browseHash(node.Name), 20, recipe), &BlobPhysical{Encoding: "raw", StoredBytes: 20})
		require.NoError(t, err)
	}
	page, err := s.MissingPhotoQualityTargetsAfter(ctx, "", 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	next, err := s.MissingPhotoQualityTargetsAfter(ctx, page[0].VersionID, 1)
	require.NoError(t, err)
	require.Len(t, next, 1)
	require.ElementsMatch(t, []PhotoVisualPreviewTarget{qualityTarget(first), qualityTarget(second)}, append(page, next...))
	scores := document.PhotoQualitySignals{Focus: .7, Blur: .3, Brightness: .5, ColorRed: .2, ColorGreen: .4, ColorBlue: .6, Framing: .8, Aesthetics: .9, FaceReports: []document.PhotoFaceReport{}}
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(first), scores, ""))
	require.NoError(t, s.PublishPhotoQualityUnavailable(ctx, qualityTarget(second), ""))
	// A terminal result is not overwritten by another worker for the same inputs.
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(second), scores, ""))
	read, err := s.photoQualityForVersions(ctx, s.db, []string{first.CurrentVersionID, second.CurrentVersionID})
	require.NoError(t, err)
	require.Equal(t, scores, read.signals[first.CurrentVersionID])
	require.True(t, read.unavailable[second.CurrentVersionID])
	require.NotContains(t, read.signals, second.CurrentVersionID)
	page, err = s.MissingPhotoQualityTargetsAfter(ctx, "", 10)
	require.NoError(t, err)
	require.Empty(t, page)
	// An evaluator upgrade replaces the old cache instead of accumulating rows.
	agePhotoQualityFixture(t, s, first.CurrentVersionID, fakeHash("ef"))
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(first), scores, ""))
	var count int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_quality_signals WHERE content_version_id=$1`, first.CurrentVersionID).Scan(&count))
	require.Equal(t, 1, count)
	updated, _, err := s.ReplaceContent(ctx, first.ID, first.Revision, browseHash("quality-replacement"), 20, "image/jpeg")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(ctx, updated.ID, updated.Revision, VersionPruneSelector{VersionIDs: []string{first.CurrentVersionID}}, true)
	require.NoError(t, err)
	read, err = s.photoQualityForVersions(ctx, s.db, []string{first.CurrentVersionID})
	require.NoError(t, err)
	require.Empty(t, read.signals)
	checkRetentionMetadata(t, s)
}

func checkPhotoQualityDetectedInputs(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", browseHash("quality-detection"), "image/jpeg")
	observation := document.FaceObservation{Box: document.FaceBox{X: .1, Y: .2, Width: .3, Height: .4}, Facing: new(.8), EyesOpen: new(false)}
	publish := func(salt string) {
		request := photoVisionJobRequest(t, s, node.CurrentVersionID, fakeHash(salt), false, document.FaceDetectionProviderID)
		grantRenditionJobConsent(t, s, request)
		at := time.Now().UTC().Add(time.Minute)
		_, _, claim := startPhotoDescription(t, s, request, at)
		result, err := s.PublishPhotoDescription(ctx, claim, faceDetectionResult(t, observation), at)
		require.NoError(t, err)
		require.Equal(t, 1, result.PublishedWaiterCount)
	}
	publish("d1")
	faces, inputs, err := s.PhotoQualityFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 1)
	require.NotEmpty(t, inputs)
	scores := document.PhotoQualitySignals{Focus: .7, FaceReports: []document.PhotoFaceReport{{FaceID: faces[0].FaceID, Sharpness: new(.8), Prominence: .25}}}
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(node), scores, inputs))
	read, err := s.photoQualityForVersions(ctx, s.db, []string{node.CurrentVersionID})
	require.NoError(t, err)
	require.Len(t, read.signals[node.CurrentVersionID].FaceReports, 1)
	observation.EyesOpen = new(true)
	publish("d2")
	_, changedInputs, err := s.PhotoQualityFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.NotEqual(t, inputs, changedInputs)
	targets, err := s.MissingPhotoQualityTargetsAfter(ctx, "", 10)
	require.NoError(t, err)
	require.Equal(t, []PhotoVisualPreviewTarget{qualityTarget(node)}, targets)
	read, err = s.photoQualityForVersions(ctx, s.db, []string{node.CurrentVersionID})
	require.NoError(t, err)
	require.True(t, read.staleFaces[node.CurrentVersionID])
	require.Empty(t, read.signals[node.CurrentVersionID].FaceReports)
	require.InDelta(t, .7, read.signals[node.CurrentVersionID].Focus, 1e-12)
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(node), scores, changedInputs))
	targets, err = s.MissingPhotoQualityTargetsAfter(ctx, "", 10)
	require.NoError(t, err)
	require.Empty(t, targets)
	// A delayed result must not replace a newer, already published face report.
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(node), scores, inputs))
	targets, err = s.MissingPhotoQualityTargetsAfter(ctx, "", 10)
	require.NoError(t, err)
	require.Empty(t, targets, "stale publication must leave the current cache intact")
}

func checkPhotoHiddenLifecyclePostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	s.photoHiddenNow = func() time.Time { return now }
	asset := albumAsset(t, s, "hidden.jpg")
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	asset, err := s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	_, err = s.PhotoAssetByID(ctx, asset.ID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	// Failures commit even though the operation returns a denial.
	for range 4 {
		_, _, err = s.UnlockPhotoHidden(ctx, "wrong")
		require.ErrorIs(t, err, ErrHiddenPasscode)
	}
	var lockout *HiddenLockoutError
	_, _, err = s.UnlockPhotoHidden(ctx, "wrong")
	require.ErrorAs(t, err, &lockout)
	_, _, err = s.UnlockPhotoHidden(ctx, "correct")
	require.ErrorAs(t, err, &lockout)
	state, err := s.PhotoHiddenState(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(300), *state.LockedFor)
	now = now.Add(5 * time.Minute)
	token, _, err := s.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	_, err = s.PhotoAssetByID(unlocked, asset.ID)
	require.NoError(t, err)
	now = now.Add(5 * time.Minute)
	_, err = s.PhotoAssetByID(unlocked, asset.ID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	for _, revoke := range []func(context.Context) error{s.LockPhotoHidden, func(ctx context.Context) error { return s.ChangePhotoHidden(ctx, "correct", "correct") }, s.ResetPhotoHidden} {
		token, _, err = s.UnlockPhotoHidden(ctx, "correct")
		require.NoError(t, err)
		require.NoError(t, revoke(ctx))
		_, err = s.PhotoAssetByID(WithPhotoHiddenToken(ctx, token), asset.ID)
		require.ErrorIs(t, err, ErrHiddenLocked)
	}
	stored, err := photoAssetByIDQuery(ctx, s.db, asset.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.HiddenAt, "reset removes credentials, not the hidden marker")
	require.NoError(t, s.SetupPhotoHidden(ctx, "replacement"))
	require.NoError(t, s.DisablePhotoHidden(ctx, "replacement"))
	stored, err = s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	require.Nil(t, stored.HiddenAt)
	checkRetentionMetadata(t, s)
}

func checkPhotoQualityHiddenRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	node := browsePhotoNode(t, s, "retained.jpg", browseHash("quality-rollback"), "image/jpeg")
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(node), document.PhotoQualitySignals{Focus: .6}, ""))
	agePhotoQualityFixture(t, s, node.CurrentVersionID, fakeHash("e1"))
	_, err := setup.ExecContext(ctx, fmt.Sprintf(`REVOKE INSERT ON documents.photo_quality_signals FROM %q`, role))
	require.NoError(t, err)
	require.ErrorContains(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(node), document.PhotoQualitySignals{Focus: .9}, ""), "permission denied")
	var fingerprint string
	var focus float64
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT evaluator_fingerprint,focus FROM photo_quality_signals WHERE content_version_id=$1`, node.CurrentVersionID).Scan(&fingerprint, &focus))
	require.Equal(t, fakeHash("e1"), fingerprint)
	require.InDelta(t, .6, focus, 1e-12)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	asset, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	before, err := s.PhotoHiddenState(WithPhotoHiddenToken(ctx, token))
	require.NoError(t, err)
	queue := xmpTargets(t, s)
	_, err = setup.ExecContext(ctx, fmt.Sprintf(`REVOKE INSERT ON documents.photo_change_receipts FROM %q`, role))
	require.NoError(t, err)
	require.ErrorContains(t, s.DisablePhotoHidden(ctx, "correct"), "permission denied")
	after, err := s.PhotoHiddenState(WithPhotoHiddenToken(ctx, token))
	require.NoError(t, err)
	require.True(t, after.Configured)
	require.NotNil(t, after.ExpiresAt)
	require.Equal(t, before.ChangeID, after.ChangeID)
	unchanged, err := s.PhotoAssetByID(WithPhotoHiddenToken(ctx, token), asset.ID)
	require.NoError(t, err)
	require.Equal(t, asset, unchanged)
	require.Equal(t, queue, xmpTargets(t, s))
}

// Seed a cache from an older evaluator without broadening runtime UPDATE rights.
func agePhotoQualityFixture(t *testing.T, s *Store, version, evaluator string) {
	t.Helper()
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO photo_quality_signals(content_version_id,evaluator_fingerprint,state,`+photoQualityColumns+`,face_reports_json,face_inputs)
 SELECT content_version_id,$1,state,`+photoQualityColumns+`,face_reports_json,face_inputs FROM photo_quality_signals WHERE content_version_id=$2`, evaluator, version); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `DELETE FROM photo_quality_signals WHERE content_version_id=$1 AND evaluator_fingerprint<>$2`, version, evaluator)
		return err
	}))
}

func checkPhotoQualityHiddenSnapshotsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "snapshot.jpg", browseHash("quality-snapshot"), "image/jpeg")
	request := photoVisionJobRequest(t, s, node.CurrentVersionID, fakeHash("a1"), false, document.FaceDetectionProviderID)
	grantRenditionJobConsent(t, s, request)
	at := time.Now().UTC().Add(time.Minute)
	_, _, claim := startPhotoDescription(t, s, request, at)
	observation := document.FaceObservation{Box: document.FaceBox{Width: .5, Height: .5}, Smile: new(false)}
	_, err := s.PublishPhotoDescription(ctx, claim, faceDetectionResult(t, observation), at)
	require.NoError(t, err)
	faces, inputs, err := s.PhotoQualityFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 1)
	request = photoVisionJobRequest(t, s, node.CurrentVersionID, fakeHash("a2"), false, document.FaceDetectionProviderID)
	grantRenditionJobConsent(t, s, request)
	_, _, claim = startPhotoDescription(t, s, request, at)
	reader := postgresSnapshotReader(t, s, func() {
		observation.Smile = new(true)
		_, err := s.PublishPhotoDescription(ctx, claim, faceDetectionResult(t, observation), at)
		require.NoError(t, err)
	})
	during, duringInputs, err := reader.PhotoQualityFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, faces, during)
	require.Equal(t, inputs, duringInputs)
	after, afterInputs, err := s.PhotoQualityFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.NotEqual(t, faces, after)
	require.NotEqual(t, inputs, afterInputs)

	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	before, err := s.PhotoHiddenState(ctx)
	require.NoError(t, err)
	hiddenReader := postgresSnapshotReader(t, s, func() { require.NoError(t, s.DisablePhotoHidden(ctx, "correct")) })
	held, err := hiddenReader.PhotoHiddenState(ctx)
	require.NoError(t, err)
	require.Equal(t, before, held, "change ID and configured state share one snapshot")
	current, err := s.PhotoHiddenState(ctx)
	require.NoError(t, err)
	require.False(t, current.Configured)
	require.NotEqual(t, before.ChangeID, current.ChangeID)
}

func checkPhotoQualityRetainedDismissalPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	source := newTestStore(t)
	node, face := faceForDecisionTest(t, source, "")
	decideFaceForTest(t, source, node, face.FaceID, "", "dismiss")
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	copyStoreCatalogFixture(t, source, setup, postgresEnrollmentCatalogs)
	faces, inputs, err := s.PhotoQualityFaces(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	require.Empty(t, faces)
	require.Empty(t, inputs, "dismissed faces must not remain in the publication fence")
	require.NoError(t, s.PublishPhotoQualitySignals(t.Context(), qualityTarget(node), document.PhotoQualitySignals{Focus: .4}, inputs))
	read, err := s.photoQualityForVersions(t.Context(), s.db, []string{node.CurrentVersionID})
	require.NoError(t, err)
	require.InDelta(t, .4, read.signals[node.CurrentVersionID].Focus, 1e-12)
	require.False(t, read.staleFaces[node.CurrentVersionID])
}
