package store

import (
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkPhotoReviewRepresentatives(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	ids := burstPhotos(t, s, 3)
	request := bestShotRequest(t, s, ids)
	request.Decision = "all"
	receipt, err := s.DecidePhotoStack(ctx, request)
	require.NoError(t, err)
	for _, id := range ids {
		asset, err := s.PhotoAssetByID(ctx, id)
		require.NoError(t, err)
		require.Equal(t, "pick", asset.Files[0].Flag)
	}
	_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	choose := func(id string) PhotoAuthoredReceipt {
		request := bestShotRequest(t, s, ids)
		request.Decision, request.CurrentID = "representative", id
		receipt, err := s.DecidePhotoStack(ctx, request)
		require.NoError(t, err)
		return receipt
	}
	first := choose(ids[0])
	second := choose(ids[1])
	require.EqualValues(t, 1, *first.Representatives[0].After)
	require.EqualValues(t, 2, *second.Representatives[0].After)
	undo, err := s.UndoPhotoAuthored(ctx, second.ReceiptID)
	require.NoError(t, err)
	require.Nil(t, undo.Representatives[0].After)
	third := choose(ids[1])
	require.EqualValues(t, 3, *third.Representatives[0].After, "undo never reuses an issued rank")
	_, err = s.UndoPhotoAuthored(ctx, undo.ReceiptID)
	require.ErrorIs(t, err, ErrStaleRevision)
	for _, id := range ids {
		asset, err := s.PhotoAssetByID(ctx, id)
		require.NoError(t, err)
		require.Empty(t, asset.Files[0].Flag, "representative choices leave file flags alone")
	}
	// Asset revisions participate in the export input even when flags did not change.
	_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: ids[:1]}), Patch: PhotoAuthoredPatch{Rating: new(3)}})
	require.NoError(t, err)
	file := bestShotPreparation(t, s, ids[:1]).Targets[0].FileID
	work, run, err := s.ClassifyPhotoXMP(ctx, file)
	require.NoError(t, err)
	require.True(t, run)
	plan, _ := PlanPhotoXMP(work, work.Steps(), "")
	plan.Packet = PhotoXMPPacket{Rating: 3, Present: PhotoConfirmedRating, Keywords: []string{}}
	plan.Write = true
	_, err = s.PublishPhotoXMP(ctx, plan, fakeHash("d4"), 8, BlobPhysical{Encoding: "raw", StoredBytes: 8, Created: true})
	require.NoError(t, err)
	_, run, err = s.ClassifyPhotoXMP(ctx, file)
	require.NoError(t, err)
	require.False(t, run)
	target := PhotoSidecarTarget{FileID: file, NodeID: work.Node.ID, AssetID: ids[0]}
	require.NotContains(t, xmpTargets(t, s), target)
	choice := choose(ids[0])
	_, run, err = s.ClassifyPhotoXMP(ctx, file)
	require.NoError(t, err)
	require.True(t, run)
	require.Contains(t, xmpTargets(t, s), target)
	_, err = s.UndoPhotoAuthored(ctx, choice.ReceiptID)
	require.NoError(t, err)
	require.Contains(t, xmpTargets(t, s), target)
	seedInitialAuditAuthority(t, s, s.RootID())
	audited := choose(ids[2])
	checkRetentionMetadata(t, s)
	_, err = s.UndoPhotoAuthored(ctx, audited.ReceiptID)
	require.NoError(t, err)
	checkRetentionMetadata(t, s)
}

func checkPhotoReviewHidden(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	ids := burstPhotos(t, s, 2)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	for _, id := range ids {
		asset, err := s.PhotoAssetByID(ctx, id)
		require.NoError(t, err)
		_, err = s.SetPhotoAssetHidden(ctx, id, asset.Revision, true)
		require.NoError(t, err)
	}
	groups, unavailable, _, _, err := s.PhotoStackGroups(ctx, ids, true, "")
	require.ErrorIs(t, err, ErrHiddenLocked)
	require.Empty(t, groups)
	require.Empty(t, unavailable)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	groups, unavailable, _, _, err = s.PhotoStackGroups(unlocked, ids, true, "")
	require.NoError(t, err)
	require.Equal(t, [][]string{ids}, groups)
	require.Empty(t, unavailable)
	request := PhotoStackDecision{BurstGapSeconds: 2, Hidden: true, AssetIDs: ids, CurrentID: ids[0], Decision: "current",
		Targets: preparedPhotoTargets(unlocked, t, s, PhotoSetSelection{AssetIDs: ids, Hidden: true})}
	receipt, err := s.DecidePhotoStack(unlocked, request)
	require.NoError(t, err)
	require.NoError(t, s.LockPhotoHidden(ctx))
	_, err = s.UndoPhotoAuthored(unlocked, receipt.ReceiptID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, err = s.DecidePhotoStack(unlocked, request)
	require.ErrorIs(t, err, ErrHiddenLocked)
	token, _, err = s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked = WithPhotoHiddenToken(ctx, token)
	_, err = s.UndoPhotoAuthored(unlocked, receipt.ReceiptID)
	require.NoError(t, err)
	for _, id := range ids {
		asset, err := s.PhotoAssetByID(unlocked, id)
		require.NoError(t, err)
		require.Empty(t, asset.Files[0].Flag)
	}
}

func checkPhotoReviewRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	for _, operation := range []string{"current", "best-shots", "representative", "undo"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			setup, owner := pgtest.NewDatabase(t)
			s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
			ctx := t.Context()
			ids := burstPhotos(t, s, 2)
			if operation == "best-shots" {
				seedBestShots(t, s, ids)
			}
			seedInitialAuditAuthority(t, s, s.RootID())
			request := bestShotRequest(t, s, ids)
			if operation != "best-shots" {
				request.Decision = operation
			}
			var undoID string
			if operation == "undo" {
				request.Decision = "representative"
				receipt, err := s.DecidePhotoStack(ctx, request)
				require.NoError(t, err)
				undoID = receipt.ReceiptID
			}
			before := bestShotPreparation(t, s, ids)
			burst, err := photoBurstSettingsTx(ctx, s.db)
			require.NoError(t, err)
			queue := xmpTargets(t, s)
			var receipts int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts`).Scan(&receipts))
			var role string
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
			_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents.photo_change_receipts FROM "+pgx.Identifier{role}.Sanitize())
			require.NoError(t, err)
			events := watchPhotoChanges(t, s)
			if operation == "undo" {
				_, err = s.UndoPhotoAuthored(ctx, undoID)
			} else {
				_, err = s.DecidePhotoStack(ctx, request)
			}
			require.ErrorContains(t, err, "permission denied for table photo_change_receipts")
			require.Empty(t, events)
			require.Equal(t, before, bestShotPreparation(t, s, ids))
			afterBurst, err := photoBurstSettingsTx(ctx, s.db)
			require.NoError(t, err)
			require.Equal(t, burst, afterBurst)
			require.Equal(t, queue, xmpTargets(t, s))
			var after int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts`).Scan(&after))
			require.Equal(t, receipts, after)
			checkRetentionMetadata(t, s)
		})
	}
}

func checkPhotoReviewSnapshotPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ids := burstPhotos(t, s, 2)
	asset, err := s.PhotoAssetByID(t.Context(), ids[1])
	require.NoError(t, err)
	var writeErr error
	reader := postgresSnapshotReader(t, s, func() {
		_, writeErr = s.SetPhotoAssetExcluded(t.Context(), asset.ID, asset.Revision, true)
	})
	groups, unavailable, _, _, err := reader.PhotoStackGroups(t.Context(), ids, false, "")
	require.NoError(t, err)
	require.NoError(t, writeErr)
	require.Equal(t, [][]string{ids}, groups)
	require.Empty(t, unavailable)
	groups, unavailable, _, _, err = s.PhotoStackGroups(t.Context(), ids, false, "")
	require.NoError(t, err)
	require.Empty(t, groups)
	require.Equal(t, ids[1:], unavailable)
}

func checkPhotoReviewRetainedVectorsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	source := newTestStore(t)
	source.vaultID = s.vaultID
	_, err := source.writeDB.ExecContext(t.Context(), `UPDATE vault_metadata SET vault_uid=$1`, source.vaultID)
	require.NoError(t, err)
	f := seedImageEmbeddingCatalog(t, source)
	ctx := t.Context()
	run, err := f.s.BeginIngest(ctx, "cli", "synthetic vector grouping")
	require.NoError(t, err)
	var ids []string
	index := newCursorIndexFixture(f)
	for i := range 2 {
		id := stackPhoto(t, f.s, run, fmt.Sprintf("/photos/frame-%d.jpg", i), fmt.Sprintf("2024-06-10T12:00:%02dZ", i*10), document.SourceMetadataPrecisionSecond)
		ids = append(ids, id)
		index.publish(t, id, []float64{1, 0, 0, 0, 0, 0, 0, 0})
	}
	index.generation(t, "retained-review-index")
	// Copy retained indexes; this qualifies their reader, not PostgreSQL index production.
	tables := append(slices.Clone(postgresEnrollmentCatalogs), "photo_source_claims", "photo_timeline", "vector_index_generations", "vector_index_heads")
	copyStoreCatalogFixture(t, f.s, setup, tables)
	groups, unavailable, _, _, err := s.PhotoStackGroups(ctx, ids, false, "")
	require.NoError(t, err)
	require.Empty(t, unavailable)
	require.Equal(t, [][]string{ids}, groups, "ten-second-separated frames require the retained vectors to group")
	request := bestShotRequest(t, s, ids)
	request.Decision = "current"
	_, err = s.DecidePhotoStack(ctx, request)
	require.NoError(t, err)
	// A corrupt disposable index falls back to capture-time grouping, not stale cached vectors.
	_, err = setup.ExecContext(ctx, `UPDATE documents.vector_index_generations SET generation_bytes=$1,byte_size=$2`, []byte("invalid-index"), len("invalid-index"))
	require.NoError(t, err)
	s.photoStackVectors = nil
	groups, unavailable, _, _, err = s.PhotoStackGroups(ctx, ids, false, "")
	require.NoError(t, err)
	require.Empty(t, groups)
	require.Empty(t, unavailable)
}
