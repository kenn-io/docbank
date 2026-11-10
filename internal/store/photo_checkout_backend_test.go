package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkoutForCommit(t *testing.T, s *Store) (PhotoCheckout, PhotoCheckoutEntry, Node) {
	t.Helper()
	ctx := t.Context()
	asset := albumAsset(t, s, "working.jpg")
	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	c, op, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, t.TempDir(), "directory-identity", 0)
	require.NoError(t, err)
	require.NoError(t, s.ClaimPhotoCheckout(ctx, c.ID, op.ID))
	entries, err := s.PhotoCheckoutEntries(ctx, c.ID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NoError(t, s.PublishPhotoCheckoutEntry(ctx, entries[0], func() (bool, error) { return true, nil }))
	require.NoError(t, s.FinishPhotoCheckout(ctx, c.ID, op.ID, StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
	c, op, err = s.PreparePhotoCheckoutOperation(ctx, PhotoCheckoutInspection{CheckoutID: c.ID}, StorageOperationKindPhotoCheckoutCommit)
	require.NoError(t, err)
	_, err = s.ClaimStorageOperation(ctx, op.ID)
	require.NoError(t, err)
	return c, entries[0], node
}

func checkPhotoCheckoutCommitAndRebind(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	c, original, node := checkoutForCommit(t, s)
	hash := browseHash("working edit")
	observation := PhotoCheckoutObservation{FileID: original.FileID, Identity: "file-identity", Hash: hash, Size: 23, State: "edited"}
	require.NoError(t, s.ObservePhotoCheckoutEntries(ctx, c.ID, c.DestinationIdentity, []PhotoCheckoutObservation{observation}))
	counts, _, err := s.PhotoCheckoutWorkingSummary(ctx, c.ID, 10)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"edited": 1}, counts)
	committed, err := s.CheckPhotoCheckoutCommitEntry(ctx, c, original)
	require.NoError(t, err)
	require.False(t, committed)
	physical := BlobPhysical{Encoding: "raw", StoredBytes: 23}
	require.NoError(t, s.CommitPhotoCheckoutEntry(ctx, c, original, hash, 23, "image/jpeg", physical))
	first, err := s.NodeByID(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.CommitPhotoCheckoutEntry(ctx, c, original, hash, 23, "image/jpeg", physical))
	committed, err = s.CheckPhotoCheckoutCommitEntry(ctx, c, original)
	require.NoError(t, err)
	require.True(t, committed)
	changed, err := s.NodeByID(ctx, node.ID)
	require.NoError(t, err)
	require.Equal(t, first, changed)
	require.Greater(t, changed.Revision, node.Revision)
	require.Equal(t, hash, changed.BlobHash)
	entries, err := s.PhotoCheckoutEntries(ctx, c.ID)
	require.NoError(t, err)
	require.Equal(t, changed.CurrentVersionID, entries[0].VersionID)
	require.Equal(t, "clean", entries[0].WorkingState)
	op, err := s.StorageOperation(ctx, c.LastOperationID)
	require.NoError(t, err)
	require.EqualValues(t, 1, op.CopiedObjects)
	require.EqualValues(t, 23, op.CopiedBytes)
	require.NoError(t, s.ReportPhotoCheckoutProgress(ctx, op.ID, 1, `{"files":1}`))
	require.NoError(t, s.FinishPhotoCheckoutOperation(ctx, c, StorageOperationCompleted, `{"files":1}`, ""))
	c, op, err = s.PreparePhotoCheckoutOperation(ctx, PhotoCheckoutInspection{CheckoutID: c.ID}, StorageOperationKindPhotoCheckoutRebind)
	require.NoError(t, err)
	_, err = s.ClaimStorageOperation(ctx, op.ID)
	require.NoError(t, err)
	require.NoError(t, s.BindPhotoCheckout(ctx, c.ID, t.TempDir(), "rebound-identity", op.ID, []PhotoCheckoutObservation{observation}))
	require.NoError(t, s.FinishPhotoCheckoutOperation(ctx, c, StorageOperationCompleted, `{"files":1}`, ""))
	rebound, err := s.PhotoCheckout(ctx, c.ID)
	require.NoError(t, err)
	require.Equal(t, "rebound-identity", rebound.DestinationIdentity)
	require.ErrorIs(t, s.ObservePhotoCheckoutEntries(ctx, c.ID, c.DestinationIdentity, []PhotoCheckoutObservation{observation}), ErrInvalidPhotoCheckout)
	retired, err := s.RetirePhotoCheckout(ctx, c.ID)
	require.NoError(t, err)
	require.NotNil(t, retired.RetiredAt)
	require.False(t, retired.DestinationReserved)
	require.NoError(t, s.ReleaseCheckoutDestination(ctx, c.ID))
	checkRetentionMetadata(t, s)
}

func checkPhotoCheckoutCommitRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	c, e, node := checkoutForCommit(t, s)
	before, err := s.PhotoCheckoutEntries(ctx, c.ID)
	require.NoError(t, err)
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	_, err = setup.ExecContext(ctx, "REVOKE UPDATE(version_id) ON documents.photo_checkout_entries FROM "+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	hash := browseHash("rejected checkout edit")
	require.ErrorContains(t, s.CommitPhotoCheckoutEntry(ctx, c, e, hash, 23, "image/jpeg", BlobPhysical{Encoding: "raw", StoredBytes: 23}), "permission denied")
	after, err := s.NodeByID(ctx, node.ID)
	require.NoError(t, err)
	require.Equal(t, node, after)
	entries, err := s.PhotoCheckoutEntries(ctx, c.ID)
	require.NoError(t, err)
	require.Equal(t, before, entries)
	committed, err := s.CheckPhotoCheckoutCommitEntry(ctx, c, e)
	require.NoError(t, err)
	require.False(t, committed)
	var blobs int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM blobs WHERE hash=$1`, hash).Scan(&blobs))
	require.Zero(t, blobs)
	op, err := s.StorageOperation(ctx, c.LastOperationID)
	require.NoError(t, err)
	require.Zero(t, op.CopiedObjects)
	_, err = setup.ExecContext(ctx, "GRANT UPDATE(version_id) ON documents.photo_checkout_entries TO "+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, s.CommitPhotoCheckoutEntry(ctx, c, e, hash, 23, "image/jpeg", BlobPhysical{Encoding: "raw", StoredBytes: 23}))
	checkRetentionMetadata(t, s)
}

func checkPhotoCheckoutCommitSnapshotPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	c, e, _ := checkoutForCommit(t, s)
	conn, err := s.db.Conn(ctx)
	require.NoError(t, err)
	var cfg *pgx.ConnConfig
	err = conn.Raw(func(driver any) error {
		pgconn, ok := driver.(*stdlib.Conn)
		require.True(t, ok)
		cfg = pgconn.Conn().Config().Copy()
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	cfg.Tracer = &afterFirstSelect{run: func() {
		require.NoError(t, s.CommitPhotoCheckoutEntry(ctx, c, e, browseHash("concurrent checkout edit"), 23, "image/jpeg", BlobPhysical{Encoding: "raw", StoredBytes: 23}))
	}}
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	reader := &Store{db: db, postgresSchema: s.postgresSchema}
	committed, err := reader.CheckPhotoCheckoutCommitEntry(ctx, c, e)
	require.NoError(t, err)
	require.False(t, committed, "admission reads the state before the concurrent commit")
	committed, err = s.CheckPhotoCheckoutCommitEntry(ctx, c, e)
	require.NoError(t, err)
	require.True(t, committed, "the next admission sees the completed commit")
}

func checkPhotoCheckoutObservationBatchesPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	for i := range 258 {
		albumAsset(t, s, fmt.Sprintf("photo-%03d.jpg", i))
	}
	c, _, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{All: true}, "checkout", "directory-identity", 1<<30)
	require.NoError(t, err)
	entries, err := s.PhotoCheckoutEntries(ctx, c.ID)
	require.NoError(t, err)
	var observations []PhotoCheckoutObservation
	for _, e := range entries {
		observations = append(observations, PhotoCheckoutObservation{FileID: e.FileID, Identity: "copy", Hash: e.Hash, Size: e.Size, State: "edited"})
	}
	require.NoError(t, s.ObservePhotoCheckoutEntries(ctx, c.ID, c.DestinationIdentity, observations))
	counts, _, err := s.PhotoCheckoutWorkingSummary(ctx, c.ID, 10)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"clean": 258}, counts)
	observations[0].State = "missing"
	observations[257].Identity = strings.Repeat("x", 128<<10)
	require.ErrorContains(t, s.ObservePhotoCheckoutEntries(ctx, c.ID, c.DestinationIdentity, observations), "batch exceeds")
	counts, _, err = s.PhotoCheckoutWorkingSummary(ctx, c.ID, 10)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"clean": 258}, counts, "the first batch rolls back when the second cannot be recorded")
}

func checkPhotoCheckoutAlbumSelectionPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	included := albumAsset(t, s, "included.jpg")
	excluded := albumAsset(t, s, "excluded.jpg")
	other := albumAsset(t, s, "other.jpg")
	_, err := s.SetPhotoAssetExcluded(ctx, excluded.ID, excluded.Revision, true)
	require.NoError(t, err)
	// An existing album is input to checkout selection; this does not qualify
	// the separate album editing or query-selection operations.
	setID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = setup.ExecContext(ctx, `INSERT INTO documents.photo_sets(set_id,name,created_at,updated_at) VALUES($1,'Album',$2,$2)`, setID, nowRFC3339())
	require.NoError(t, err)
	for _, asset := range []PhotoAsset{included, excluded} {
		_, err = setup.ExecContext(ctx, `INSERT INTO documents.photo_set_members(set_id,asset_id,added_at) VALUES($1,$2,$3)`, setID, asset.ID, nowRFC3339())
		require.NoError(t, err)
	}
	c, _, estimate, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AlbumIDs: []string{setID, setID}, AssetIDs: []string{other.ID}}, t.TempDir(), "identity", 0)
	require.NoError(t, err)
	require.EqualValues(t, 2, estimate.Assets)
	entries, err := s.PhotoCheckoutEntries(ctx, c.ID)
	require.NoError(t, err)
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.AssetID)
	}
	require.ElementsMatch(t, []string{included.ID, other.ID}, ids)
	_, err = setup.ExecContext(ctx, `UPDATE documents.photo_sets SET deleted_at=$1 WHERE set_id=$2`, nowRFC3339(), setID)
	require.NoError(t, err)
	_, err = s.EstimatePhotoCheckout(ctx, PhotoCheckoutSelection{AlbumIDs: []string{setID}})
	require.ErrorIs(t, err, ErrNotFound)
}

func checkPhotoCheckoutPublicationRecoveryPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	asset := albumAsset(t, s, "copy.jpg")
	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	c, op, _, err := s.CreatePhotoCheckout(ctx, PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, t.TempDir(), "identity", 0)
	require.NoError(t, err)
	require.NoError(t, s.ClaimPhotoCheckout(ctx, c.ID, op.ID))
	entries, err := s.PhotoCheckoutEntries(ctx, c.ID)
	require.NoError(t, err)
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	_, err = setup.ExecContext(ctx, "REVOKE UPDATE(published) ON documents.photo_checkout_entries FROM "+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	called := false
	err = s.PublishPhotoCheckoutEntry(ctx, entries[0], func() (bool, error) {
		intent, err := s.StorageOperation(ctx, op.ID)
		require.NoError(t, err)
		require.Equal(t, entries[0].FileID, intent.Cursor, "intent is committed before the external copy")
		// External publication cannot roll back with the database transaction.
		called = true
		return true, nil
	})
	require.True(t, called)
	require.ErrorIs(t, err, ErrPhotoCheckoutPublicationUnrecorded)
	intent, err := s.StorageOperation(ctx, op.ID)
	require.NoError(t, err)
	require.Equal(t, entries[0].FileID, intent.Cursor)
	_, err = setup.ExecContext(ctx, "GRANT UPDATE(published) ON documents.photo_checkout_entries TO "+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, s.FinishPhotoCheckout(ctx, c.ID, op.ID, StorageOperationFailed, "{}", "recording copy failed", time.Now().Add(time.Hour)))
	changed, _, err := s.ReplaceContent(ctx, node.ID, node.Revision, browseHash("new copy"), 23, "image/jpeg")
	require.NoError(t, err)
	pruned, err := s.PruneContentVersions(ctx, node.ID, changed.Revision, VersionPruneSelector{VersionIDs: []string{node.CurrentVersionID}}, true)
	require.NoError(t, err)
	require.Zero(t, pruned.DeletedVersions)
	require.Len(t, pruned.DependencyRetained, 1)
	_, err = s.RetirePhotoCheckout(ctx, c.ID)
	require.NoError(t, err)
	pruned, err = s.PruneContentVersions(ctx, node.ID, changed.Revision, VersionPruneSelector{VersionIDs: []string{node.CurrentVersionID}}, true)
	require.NoError(t, err)
	require.Equal(t, 1, pruned.DeletedVersions)
}
