package store

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/stdlib"
	"go.kenn.io/docbank/document"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkPhotoXMPPublication(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "capture.jpg")
	file := asset.Files[0]
	publishXMPOrientation(t, s, file.NodeID, fakeHash("ee"))
	_, err := editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: file.ID, Revision: 1, Patch: PhotoAuthoredPatch{Caption: new("River"), Rating: new(3)}}})
	require.NoError(t, err)
	work, run, err := s.ClassifyPhotoXMP(ctx, file.ID)
	require.NoError(t, err)
	require.True(t, run)
	require.Nil(t, work.Sidecar)
	plan := PhotoXMPPlan{Work: work, Values: work.Original.Authored(), Keywords: []string{}, Packet: PhotoXMPPacket{Rating: 3, Caption: "River", Present: PhotoConfirmedRating | PhotoConfirmedCaption, Keywords: []string{}}, Write: true}
	_, err = s.PublishPhotoXMP(ctx, plan, fakeHash("b2"), 12, BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 12})
	require.NoError(t, err)
	require.Equal(t, PhotoXMPStateSynced, xmpState(t, s, file.ID).State)
	require.Empty(t, xmpTargets(t, s))
	_, err = s.PublishPhotoXMP(ctx, plan, fakeHash("c3"), 13, BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 13})
	require.ErrorIs(t, err, ErrStaleRevision, "a second worker cannot publish an already settled plan")
	settled, run, err := s.ClassifyPhotoXMP(ctx, file.ID)
	require.NoError(t, err)
	require.False(t, run)
	require.NotNil(t, settled.Sidecar)
	require.Equal(t, "capture.jpg.xmp", settled.Sidecar.Node.Name)
	require.Equal(t, fakeHash("b2"), settled.Sidecar.Node.BlobHash)
	require.NoError(t, s.CheckPhotoXMPPairing(ctx, settled, "jpg"))
	checkRetentionMetadata(t, s)

	// A later edit replaces the sidecar; the previous packet stays retained.
	_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: file.ID, Revision: settled.Original.Revision, Patch: PhotoAuthoredPatch{Rating: new(5)}}})
	require.NoError(t, err)
	work, run, err = s.ClassifyPhotoXMP(ctx, file.ID)
	require.NoError(t, err)
	require.True(t, run)
	plan, mask := planXMP(work, settled.History.Settled.Packet, "")
	require.NotZero(t, mask)
	plan.Write = true
	plan.Packet.Rating = 5
	_, err = s.PublishPhotoXMP(ctx, plan, fakeHash("c3"), 13, BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 13})
	require.NoError(t, err)
	require.Empty(t, xmpTargets(t, s))
	prior, err := s.ContentVersionByID(ctx, settled.Sidecar.Node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, fakeHash("b2"), prior.BlobHash)

	// A detached generated sidecar is reused after an edit, without a second node.
	asset, err = s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, settled.Sidecar.FileID, PhotoDetachOptions{})
	require.NoError(t, err)
	_, run, err = s.ClassifyPhotoXMP(ctx, file.ID)
	require.NoError(t, err)
	require.False(t, run)
	require.Empty(t, xmpTargets(t, s))
	current, err := photoFileByIDQuery(ctx, s.db, file.ID)
	require.NoError(t, err)
	_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: file.ID, Revision: current.Revision, Patch: PhotoAuthoredPatch{Caption: new("Harbor")}}})
	require.NoError(t, err)
	resumed, run, err := s.ClassifyPhotoXMP(ctx, file.ID)
	require.NoError(t, err)
	require.True(t, run)
	require.NotNil(t, resumed.Sidecar)
	require.Equal(t, settled.Sidecar.Node.ID, resumed.Sidecar.Node.ID)
	checkRetentionMetadata(t, s)
}

func checkPhotoXMPConflictResolution(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	for _, choice := range []string{"sidecar", "catalog"} {
		t.Run(choice, func(t *testing.T) {
			s, _, file := xmpSidecarPhoto(t, newStore(t))
			ctx := t.Context()
			importXMP(t, s, file.ID, PhotoAuthored{Rating: 3, Confirmed: PhotoConfirmedRating}, "Travel")
			current, err := photoFileByIDQuery(ctx, s.db, file.ID)
			require.NoError(t, err)
			_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: file.ID, Revision: current.Revision, Patch: PhotoAuthoredPatch{Rating: new(5)}}})
			require.NoError(t, err)
			prior, _, err := s.ClassifyPhotoXMP(ctx, file.ID)
			require.NoError(t, err)
			_, _, err = s.ReplaceContent(ctx, prior.Sidecar.Node.ID, prior.Sidecar.Node.Revision, fakeHash("d4"), 50, "application/rdf+xml")
			require.NoError(t, err)
			work, run, err := s.ClassifyPhotoXMP(ctx, file.ID)
			require.NoError(t, err)
			require.True(t, run)
			packet := PhotoXMPPacket{Rating: 4, Present: PhotoConfirmedRating, Keywords: []string{"Travel"}}
			plan, _ := planXMP(work, packet, "")
			require.Equal(t, PhotoConfirmedRating, plan.Conflicts)
			_, err = s.PublishPhotoXMP(ctx, plan, "", 0, BlobPhysical{})
			require.NoError(t, err)
			conflict := xmpState(t, s, file.ID)
			require.Equal(t, PhotoXMPStateConflict, conflict.State)
			require.Equal(t, []PhotoXMPRow{{Field: "Rating", Catalog: "5", Sidecar: "4"}}, conflict.Rows)
			work, err = s.PhotoXMPConflict(ctx, file.ID, conflict.ConflictID)
			require.NoError(t, err)
			plan, mask := planXMP(work, packet, choice)
			plan.Write = mask != 0
			if plan.Write {
				plan.Packet.Rating = 5
			}
			hash, size, rating := "", int64(0), 4
			if choice == "catalog" {
				hash, size, rating = fakeHash("e5"), 51, 5
			}
			_, err = s.PublishPhotoXMP(ctx, plan, hash, size, BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: size})
			require.NoError(t, err)
			require.Equal(t, PhotoXMPStateSynced, xmpState(t, s, file.ID).State)
			require.Empty(t, xmpTargets(t, s))
			current, err = photoFileByIDQuery(ctx, s.db, file.ID)
			require.NoError(t, err)
			require.Equal(t, rating, current.Rating)
			_, err = s.PhotoXMPConflict(ctx, file.ID, conflict.ConflictID)
			require.ErrorIs(t, err, ErrStaleRevision)
			checkRetentionMetadata(t, s)
		})
	}
}

func checkPhotoEditRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	s, _, file := xmpSidecarPhoto(t, s)
	importXMP(t, s, file.ID, PhotoAuthored{Rating: 3, Confirmed: PhotoConfirmedRating}, "Travel")
	prepared, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{file.AssetID}})
	require.NoError(t, err)
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, "SELECT current_user").Scan(&role))
	role = pgx.Identifier{role}.Sanitize()
	_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents.photo_change_receipts FROM "+role)
	require.NoError(t, err)
	_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{file.AssetID}}), Patch: PhotoAuthoredPatch{Rating: new(5)}})
	require.ErrorContains(t, err, "permission denied for table photo_change_receipts")
	after, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{file.AssetID}})
	require.NoError(t, err)
	require.Equal(t, prepared, after)
	require.Empty(t, xmpTargets(t, s), "failed edits leave no export work")
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON documents.photo_change_receipts TO "+role)
	require.NoError(t, err)
	current, err := photoFileByIDQuery(ctx, s.db, file.ID)
	require.NoError(t, err)
	_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: file.ID, Revision: current.Revision, Patch: PhotoAuthoredPatch{Rating: new(5)}}})
	require.NoError(t, err)
	work, run, err := s.ClassifyPhotoXMP(ctx, file.ID)
	require.NoError(t, err)
	require.True(t, run)
	plan, mask := planXMP(work, work.History.Settled.Packet, "")
	require.NotZero(t, mask)
	plan.Write = true
	plan.Packet.Rating = 5
	_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents.photo_change_receipts FROM "+role)
	require.NoError(t, err)
	_, err = s.PublishPhotoXMP(ctx, plan, fakeHash("e7"), 20, BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 20})
	require.ErrorContains(t, err, "permission denied for table photo_change_receipts")
	node, err := s.NodeByID(ctx, work.Sidecar.Node.ID)
	require.NoError(t, err)
	require.Equal(t, work.Sidecar.Node, node)
	var count int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM blobs WHERE hash=$1`, fakeHash("e7")).Scan(&count))
	require.Zero(t, count)
	require.Equal(t, PhotoXMPStatePending, xmpState(t, s, file.ID).State)
	checkRetentionMetadata(t, s)
}

// Existing face and description records must not prevent a metadata edit. Their
// producers remain SQLite-only; all edits below run natively on each backend.
func checkPhotoEditRetainedSuggestionsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	source := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, source, "portrait.jpg")
	node := requireNodeByID(t, source, asset.Files[0].NodeID)
	publishPhotoDescription(t, source, asset, producerA, false, document.PhotoDescription{Caption: "River", Tags: []string{"Travel"}, Rotation: new(90)})
	require.NoError(t, source.withStorageTx(ctx, func(tx *sql.Tx) error {
		return publishFaceDetectionTx(ctx, tx, node.CurrentVersionID, producerA, document.FaceDetection{Faces: []document.FaceObservation{{Box: document.FaceBox{X: .1, Y: .1, Width: .2, Height: .2}, Roll: new(1.5707963267948966)}}}, time.Now())
	}))
	require.NoError(t, source.ValidateMetadata(ctx))
	target, _ := copyRetentionStore(t, source)
	selection := PhotoSetSelection{AssetIDs: []string{asset.ID}}
	before, err := source.PreparePhotoAuthored(ctx, selection)
	require.NoError(t, err)
	require.NotEmpty(t, before.Suggestions)
	actual, err := target.PreparePhotoAuthored(ctx, selection)
	require.NoError(t, err)
	require.Equal(t, before, actual)
	for _, s := range []*Store{source, target} {
		tag, err := s.TagByName(ctx, "Travel")
		if errors.Is(err, ErrNotFound) {
			tag, err = s.CreateTag(ctx, "Travel")
		}
		require.NoError(t, err)
		_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, selection), Patch: PhotoAuthoredPatch{Caption: new("River"), Rotation: new(90)}, Keywords: []PhotoKeywordEdit{{TagID: tag.ID, Assign: true}}})
		require.NoError(t, err)
		prepared, err := s.PreparePhotoAuthored(ctx, selection)
		require.NoError(t, err)
		require.Empty(t, prepared.Suggestions, "authored values suppress retained suggestions")
		checkRetentionMetadata(t, s)
	}
}

func checkPhotoPreparationSnapshotPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	asset := authoredPair(t, s)
	selection := PhotoSetSelection{AssetIDs: []string{asset.ID}}
	before, err := s.PreparePhotoAuthored(ctx, selection)
	require.NoError(t, err)
	targets := preparedPhotoTargets(ctx, t, s, selection)
	conn, err := s.db.Conn(ctx)
	require.NoError(t, err)
	var cfg *pgx.ConnConfig
	err = conn.Raw(func(raw any) error {
		pg, ok := raw.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("unexpected PostgreSQL connection %T", raw)
		}
		cfg = pg.Conn().Config().Copy()
		return nil
	})
	require.NoError(t, conn.Close())
	require.NoError(t, err)
	var changed bool
	var mutationErr error
	cfg.Tracer = &afterFirstSelect{run: func() {
		_, mutationErr = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: targets, Patch: PhotoAuthoredPatch{Caption: new("Later caption")}})
		changed = true
	}}
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	reader := &Store{db: db, rootID: s.rootID, vaultID: s.vaultID, postgresSchema: s.postgresSchema}
	during, err := reader.PreparePhotoAuthored(ctx, selection)
	require.NoError(t, mutationErr)
	require.True(t, changed)
	require.NoError(t, err)
	require.Equal(t, before, during)
	after, err := s.PreparePhotoAuthored(ctx, selection)
	require.NoError(t, err)
	for _, target := range after.Targets {
		require.Equal(t, "Later caption", target.Values.Caption)
	}
	require.NotEqual(t, before, after)
}

func checkPhotoXMPEmbeddedSourceClaim(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "Embedded:agent", "synthetic source observation")
	require.NoError(t, err)
	photo, err := s.IngestFileExact(ctx, run, s.RootID(), "capture.jpg", fakeHash("a5"), 4, "image/jpeg", "/camera/unrelated.png", "")
	require.NoError(t, err)
	sidecar, err := s.IngestFileExact(ctx, run, s.RootID(), "capture.jpg.xmp", fakeHash("b5"), 4, "application/rdf+xml", "/camera/unrelated.png.xmp", "")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, photo.ID)
	require.NoError(t, err)
	file := asset.Files[0]
	_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &file.ID)
	require.NoError(t, err)
	publishXMPOrientation(t, s, photo.ID, fakeHash("ee"))
	work, _, err := s.ClassifyPhotoXMP(ctx, file.ID)
	require.NoError(t, err)
	require.NoError(t, s.CheckPhotoXMPClaim(ctx, work, "/camera/unrelated.png.xmp", "jpg"))
	require.ErrorIs(t, s.CheckPhotoXMPClaim(ctx, work, "/camera/unrelated.png.xmp", "png"), ErrInvalidPhotoAsset)
	require.NoError(t, s.CheckPhotoXMPPairing(ctx, work, "jpg"))
	require.ErrorIs(t, s.CheckPhotoXMPPairing(ctx, work, "png"), ErrInvalidPhotoAsset)
}
