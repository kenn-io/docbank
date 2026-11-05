package store

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func faceDetectionResult(t *testing.T, faces ...document.FaceObservation) *document.RenditionResult {
	t.Helper()
	payload, err := json.Marshal(document.FaceDetection{Faces: faces})
	require.NoError(t, err)
	return &document.RenditionResult{Artifacts: []document.RenditionArtifact{{Role: document.EvidenceArtifactStructured, Payload: payload}}}
}

func checkPhotoFaceDetectionJob(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "portrait.jpg")
	node := requireNodeByID(t, s, asset.Files[0].NodeID)
	first := document.FaceObservation{Box: document.FaceBox{X: .1, Y: .1, Width: .2, Height: .2}, Roll: new(0.0), Facing: new(.9), EyesOpen: new(true), Smile: new(false)}
	other := document.FaceObservation{Box: document.FaceBox{X: .7, Y: .7, Width: .2, Height: .2}}
	var originalID string
	for i, observations := range [][]document.FaceObservation{{first, other}, {first}, {{Box: first.Box}}, {first}, {first}, {}} {
		request := photoVisionJobRequest(t, s, node.CurrentVersionID, fakeHash(fmt.Sprintf("b%d", i)), false, document.FaceDetectionProviderID)
		grantRenditionJobConsent(t, s, request)
		targets, err := s.PhotoDescriptionTargets(ctx, request.Profile.Fingerprint, request.Profile.RenditionRequestFingerprint, "", 10, false, true)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		at := time.Now().UTC().Add(time.Minute)
		_, _, claim := startPhotoDescription(t, s, request, at)
		_, checkpoint, err := s.VaultChanges(ctx, "", 500)
		require.NoError(t, err)
		published, err := s.PublishPhotoDescription(ctx, claim, faceDetectionResult(t, observations...), at)
		require.NoError(t, err)
		require.Equal(t, 1, published.PublishedWaiterCount)
		faces, _, _, err := s.PhotoFacePersonNames(ctx, node.CurrentVersionID)
		require.NoError(t, err)
		require.Len(t, faces, len(observations))
		for _, face := range faces {
			if face.Box != first.Box {
				continue
			}
			if originalID == "" {
				originalID = face.FaceID
			}
			require.Equal(t, originalID, face.FaceID, "a matched observation keeps its identity")
			require.Equal(t, observations[0].Roll, face.Roll)
			require.Equal(t, observations[0].EyesOpen, face.EyesOpen)
			require.Equal(t, observations[0].Smile, face.Smile)
		}
		changes, _, err := s.VaultChanges(ctx, checkpoint, 500)
		require.NoError(t, err)
		if i == 4 {
			require.Empty(t, changes, "a detector-only change does not announce new observations")
		} else {
			require.Len(t, changes, 1)
			require.Equal(t, "processed", changes[0].Kind)
		}
		targets, err = s.PhotoDescriptionTargets(ctx, request.Profile.Fingerprint, request.Profile.RenditionRequestFingerprint, "", 10, false, true)
		require.NoError(t, err)
		require.Empty(t, targets, "completion includes an empty result")
		joined, waiter, err := s.EnqueueRenditionJob(ctx, request)
		require.NoError(t, err)
		require.Equal(t, RenditionJobCompleted, joined.State)
		require.Equal(t, "published", waiter.State)
	}
	require.NotEmpty(t, originalID)
	checkRetentionMetadata(t, s)
}

func checkPhotoAnalysisPublicationRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for _, detection := range []bool{false, true} {
		t.Run(strconv.FormatBool(detection), func(t *testing.T) {
			setup, owner := pgtest.NewDatabase(t)
			s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
			ctx := t.Context()
			asset := albumAsset(t, s, "publish.jpg")
			node := requireNodeByID(t, s, asset.Files[0].NodeID)
			provider, table := "synthetic-rendition", "photo_suggestions"
			result := photoDescriptionResult("Harbor")
			if detection {
				provider, table = document.FaceDetectionProviderID, "photo_face_scans"
				result = faceDetectionResult(t, document.FaceObservation{Box: document.FaceBox{Width: .2, Height: .2}, Smile: new(false)})
			}
			request := photoVisionJobRequest(t, s, node.CurrentVersionID, producerB, true, provider)
			grantRenditionJobConsent(t, s, request)
			at := time.Now().UTC().Add(time.Minute)
			_, _, claim := startPhotoDescription(t, s, request, at)
			_, checkpoint, err := s.VaultChanges(ctx, "", 500)
			require.NoError(t, err)
			var role string
			require.NoError(t, s.db.QueryRowContext(ctx, "SELECT current_user").Scan(&role))
			_, err = setup.ExecContext(ctx, fmt.Sprintf(`REVOKE INSERT ON documents.%s FROM %q`, table, role))
			require.NoError(t, err)
			_, err = s.PublishPhotoDescription(ctx, claim, result, at)
			require.ErrorContains(t, err, "permission denied")
			for _, catalog := range []string{"photo_suggestions", "photo_faces", "photo_face_scans"} {
				var count int
				require.NoError(t, s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+catalog).Scan(&count))
				require.Zero(t, count, catalog)
			}
			changes, _, err := s.VaultChanges(ctx, checkpoint, 500)
			require.NoError(t, err)
			require.Empty(t, changes)
			_, err = setup.ExecContext(ctx, fmt.Sprintf(`GRANT INSERT ON documents.%s TO %q`, table, role))
			require.NoError(t, err)
			published, err := s.PublishPhotoDescription(ctx, claim, result, at)
			require.NoError(t, err, "the same live claim retries after complete rollback")
			require.Equal(t, 1, published.PublishedWaiterCount)
			changes, _, err = s.VaultChanges(ctx, checkpoint, 500)
			require.NoError(t, err)
			require.Len(t, changes, 1)
			checkRetentionMetadata(t, s)
		})
	}
}

func checkPhotoSuggestionResponseRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	asset := albumAsset(t, s, "respond.jpg")
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "Harbor", Tags: []string{"Coast"}})
	before, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	queue := xmpTargets(t, s)
	_, checkpoint, err := s.VaultChanges(ctx, "", 500)
	require.NoError(t, err)
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, "SELECT current_user").Scan(&role))
	_, err = setup.ExecContext(ctx, fmt.Sprintf(`REVOKE UPDATE(caption) ON documents.photo_suggestions FROM %q`, role))
	require.NoError(t, err)
	request := PhotoSuggestionResponse{AssetID: proposal.AssetID, AssetRevision: proposal.AssetRevision, SourceRevision: proposal.SourceRevision, NodeRevision: proposal.NodeRevision, Response: "accepted", Caption: true, Tags: []string{"Coast"}}
	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, request)
	require.ErrorContains(t, err, "permission denied")
	after, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	require.Equal(t, before, after, "authored values, revisions and pending suggestions all roll back")
	require.Equal(t, queue, xmpTargets(t, s))
	_, count, err := s.Tags(ctx, 10, 0)
	require.NoError(t, err)
	require.Zero(t, count, "the newly created tag rolls back too")
	changes, _, err := s.VaultChanges(ctx, checkpoint, 500)
	require.NoError(t, err)
	require.Empty(t, changes)
	_, err = setup.ExecContext(ctx, fmt.Sprintf(`GRANT UPDATE(caption) ON documents.photo_suggestions TO %q`, role))
	require.NoError(t, err)
	accepted, err := s.RespondPhotoSuggestion(ctx, proposal.ID, request)
	require.NoError(t, err)
	require.NotNil(t, accepted.AuthoredReceipt)
	checkRetentionMetadata(t, s)
}

func checkPhotoAnalysisTargetPlan(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	asset := albumAsset(t, s, "page.jpg")
	node := requireNodeByID(t, s, asset.Files[0].NodeID)
	request := photoDescriptionJobRequest(t, s, node.CurrentVersionID, producerB, false)
	fingerprints, err := document.CurrentPhotoQualityFingerprints()
	require.NoError(t, err)
	ctx := t.Context()
	err = s.withReadTx(ctx, func(tx *sql.Tx) error {
		prefix := "EXPLAIN QUERY PLAN "
		if s.postgresSchema != "" {
			if _, err := tx.ExecContext(ctx, "SET LOCAL enable_seqscan=off"); err != nil {
				return err
			}
			prefix = "EXPLAIN (COSTS OFF) "
		}
		rows, err := tx.QueryContext(ctx, prefix+s.photoDescriptionTargetsSQL(photoSuggestionSettledSQL("g.content_version_id", "$3", false)), "", fingerprints.GridRecipe, request.Profile.RenditionRequestFingerprint, false, request.Profile.Fingerprint, document.FaceDetectionProviderID, false, 10)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var lines []string
		for rows.Next() {
			var line string
			if s.postgresSchema != "" {
				err = rows.Scan(&line)
			} else {
				var id, parent, unused int
				err = rows.Scan(&id, &parent, &unused, &line)
			}
			if err != nil {
				return err
			}
			lines = append(lines, line)
		}
		plan := strings.Join(lines, "\n")
		require.Contains(t, plan, "visual_preview_generations_ready", "target discovery uses the ready-preview index")
		if s.postgresSchema == "" {
			require.Contains(t, plan, "SEARCH g USING INDEX visual_preview_generations_ready (recipe_fingerprint=? AND content_version_id>?)", "SQLite seeks the recipe and cursor rather than scanning previews")
		}
		return rows.Err()
	})
	require.NoError(t, err)
}
