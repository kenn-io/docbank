package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
	"go.kenn.io/kit/pack"
)

func TestDocumentWritesPostgres(t *testing.T) {
	t.Parallel()
	for _, audited := range []bool{false, true} {
		name := "ordinary"
		if audited {
			name = "audited"
		}
		t.Run(name, func(t *testing.T) {
			s := newPostgresQueryStore(t)
			ctx := t.Context()
			if audited {
				plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "cli", nil)
				require.NoError(t, err)
				_, err = s.EnableInitialAudit(ctx, plan)
				require.NoError(t, err)
			}
			// Exercise cursors beyond PostgreSQL's int4 range.
			_, err := s.db.ExecContext(ctx, `UPDATE vault_change_head SET high_water=2147483648`)
			require.NoError(t, err)
			_, before, err := s.VaultChanges(ctx, "", 500)
			require.NoError(t, err)
			dir, path, err := s.MkdirPath(ctx, "/Documents")
			require.NoError(t, err)
			require.Equal(t, "/Documents", path)
			nested, err := s.EnsureDir(ctx, dir.ID, "Drafts")
			require.NoError(t, err)
			existing, err := s.EnsureDir(ctx, dir.ID, "Drafts")
			require.NoError(t, err)
			require.Equal(t, nested, existing)
			receipt, err := s.CreateFileWithReceipt(ctx, nested.ID, "note.txt", fakeHash("a1"), 4, "text/plain")
			require.NoError(t, err)
			original := receipt.Node
			require.Equal(t, PhysicalContent{Kind: "loose", Encoding: "raw", LogicalBytes: 4, StoredBytes: 4, PackEligible: true}, receipt.Physical)
			_, err = s.CreateFile(ctx, nested.ID, "note.txt", fakeHash("b1"), 5, "text/plain")
			require.ErrorIs(t, err, ErrExists)
			var rejectedBlobs int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM blobs WHERE hash=$1`, fakeHash("b1")).Scan(&rejectedBlobs))
			require.Zero(t, rejectedBlobs)
			next, err := s.Mkdir(ctx, nested.ID, "Next")
			require.NoError(t, err)
			require.Equal(t, original.ID+1, next.ID, "failed insertion must not consume an audit node ID")
			confirmed, err := s.ConfirmContentWithReceipt(ctx, original.ID, original.Revision, original.BlobHash, original.Size, original.MimeType)
			require.NoError(t, err)
			require.Equal(t, receipt, confirmed)
			replaced, err := s.ReplaceContentWithReceipt(ctx, original.ID, original.Revision, fakeHash("a2"), 8, "text/plain")
			require.NoError(t, err)
			require.Equal(t, original.ID, replaced.Node.ID)
			require.Equal(t, int64(2), replaced.Node.Revision)
			_, err = s.ReplaceContentWithReceipt(ctx, original.ID, original.Revision, fakeHash("b2"), 12, "text/plain")
			require.ErrorIs(t, err, ErrStaleRevision)
			reverted, version, _, err := s.RevertContent(ctx, original.ID, replaced.Node.Revision, original.CurrentVersionID)
			require.NoError(t, err)
			changes, _, err := s.VaultChanges(ctx, before, 500)
			require.NoError(t, err)
			require.Len(t, changes, 6)
			require.Equal(t, []string{"created", "created", "created", "created", "version_added", "version_added"}, []string{changes[0].Kind, changes[1].Kind, changes[2].Kind, changes[3].Kind, changes[4].Kind, changes[5].Kind})
			require.Equal(t, original.ID, changes[2].NodeID)
			require.Equal(t, int64(1), changes[2].Revision)
			require.Equal(t, original.ID, changes[4].NodeID)
			require.Equal(t, int64(2), changes[4].Revision)
			require.Equal(t, int64(3), changes[5].Revision)

			require.Equal(t, original.BlobHash, reverted.BlobHash)
			require.Equal(t, "content_revert", version.TransitionKind)
			require.Equal(t, new(original.CurrentVersionID), version.SourceVersionID)
			moved, path, err := s.Move(ctx, original.ID, dir.ID, "final.txt", reverted.Revision)
			require.NoError(t, err)
			require.Equal(t, "/Documents/final.txt", path)
			require.Equal(t, int64(4), moved.Revision)
			_, _, err = s.Move(ctx, moved.ID, nested.ID, "Next", moved.Revision)
			require.ErrorIs(t, err, ErrExists)
			unchanged, err := s.NodeByID(ctx, moved.ID)
			require.NoError(t, err)
			require.Equal(t, moved, unchanged)
			directory, err := s.NodeByID(ctx, dir.ID)
			require.NoError(t, err)
			_, _, err = s.Move(ctx, dir.ID, s.RootID(), "Archive", directory.Revision)
			require.NoError(t, err)
			childPath, err := s.Path(ctx, next.ID)
			require.NoError(t, err)
			require.Equal(t, "/Archive/Drafts/Next", childPath)
			history, total, err := s.ContentVersions(ctx, original.ID, 10, 0)
			require.NoError(t, err)
			require.Equal(t, 3, total)
			require.Len(t, history, 3)
			var eligible, queued int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM text_searchable_versions`).Scan(&eligible))
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM text_extraction_queue`).Scan(&queued))
			require.Equal(t, 3, eligible)
			require.Equal(t, 2, queued)
			photo, err := s.CreateFile(ctx, dir.ID, "photo.jpg", fakeHash("a3"), 4, "image/jpeg")
			require.NoError(t, err)
			var enrolled int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM photo_files WHERE node_id=$1`, photo.ID).Scan(&enrolled))
			if audited {
				require.Zero(t, enrolled)
			} else {
				require.Equal(t, 1, enrolled)
			}
			require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				high, err := s.auditNodeSequence(ctx, tx)
				if err != nil {
					return err
				}
				return s.validateMetadataState(ctx, tx, high)
			}))
		})
	}
}

func TestDocumentNegativeSizePostgres(t *testing.T) {
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	// An otherwise valid compressed receipt must not hide an invalid logical
	// size. The omitted receipt must reject it too, before synthesizing raw bytes.
	for _, physical := range [][]BlobPhysical{nil, {{Encoding: "zstd", StoredBytes: 4, PackEligible: true}}} {
		_, err := s.CreateFile(ctx, s.RootID(), "invalid.txt", fakeHash("f1"), -1, "text/plain", physical...)
		require.ErrorContains(t, err, "content size must not be negative")
		var retained int
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM nodes WHERE parent_id IS NOT NULL) +
			(SELECT count(*) FROM blobs) +
			(SELECT count(*) FROM content_versions) +
			(SELECT count(*) FROM blob_locations)`).Scan(&retained))
		require.Zero(t, retained, "a rejected create must leave no document or physical authority")
	}
}

// The released SQLite trigger results are the oracle for retained photo facts.
// Replace/revert must remove and restore the displayed bytes' source and date;
// moving an unowned sibling sidecar must refresh its original's export work.
func TestDocumentPhotoMaintenancePostgres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	source := newTestStore(t)
	run, err := source.BeginIngest(ctx, "cli", "Synthetic photos")
	require.NoError(t, err)
	node, err := source.IngestFileExact(ctx, run, source.RootID(), "photo.jpg", fakeHash("a4"), 20, "image/jpeg", "/photos/photo.jpg", "2024-03-05T10:00:00Z")
	require.NoError(t, err)
	browsePhotoMetadata(t, source, node, "photo-exif", exifCapture("2024-06-10T12:00:00Z"))
	setup, owner := pgtest.NewDatabase(t)
	target := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	tables := append(append([]string{}, postgresEnrollmentCatalogs...), "blob_locations", "blob_packs", "blob_pack_entries", "photo_source_claims", "photo_timeline")
	copyStoreCatalogFixture(t, source, setup, tables)
	target.primaryStoreID = source.primaryStoreID
	type projection struct{ Path, Mtime, Folder, Sort, Day, Modified, TimelineFolder string }
	read := func(s *Store) projection {
		var p projection
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COALESCE(c.original_path,''), COALESCE(c.original_mtime,''), COALESCE(c.folder_key,''), COALESCE(t.sort_key,''), COALESCE(t.day,''), COALESCE(t.modified_time,''), COALESCE(t.folder_key,'') FROM photo_files f LEFT JOIN photo_source_claims c ON c.file_id=f.file_id LEFT JOIN photo_timeline t ON t.file_id=f.file_id WHERE f.node_id=$1`, node.ID).Scan(&p.Path, &p.Mtime, &p.Folder, &p.Sort, &p.Day, &p.Modified, &p.TimelineFolder))
		return p
	}
	original := read(source)
	require.Equal(t, "/photos/photo.jpg", original.Path)
	require.Equal(t, "2024-06-10", original.Day)
	for _, action := range []string{"replace", "revert"} {
		for _, s := range []*Store{source, target} {
			current, err := s.NodeByID(ctx, node.ID)
			require.NoError(t, err)
			if action == "replace" {
				_, _, err = s.ReplaceContent(ctx, node.ID, current.Revision, fakeHash("a5"), 8, "image/jpeg")
			} else {
				_, _, _, err = s.RevertContent(ctx, node.ID, current.Revision, node.CurrentVersionID)
			}
			require.NoError(t, err)
		}
		require.Equal(t, read(source), read(target), action)
		if action == "replace" {
			require.Equal(t, projection{}, read(target))
		} else {
			require.Equal(t, original, read(target))
		}
	}
}

func TestDocumentSidecarWorkPostgres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	source, asset, raw := xmpSidecarPhoto(t)
	importXMP(t, source, raw.ID, PhotoAuthored{Rating: 3, Confirmed: PhotoConfirmedRating})
	require.Empty(t, xmpTargets(t, source))
	setup, owner := pgtest.NewDatabase(t)
	target := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	tables := append(append([]string{}, postgresEnrollmentCatalogs...), "blob_locations", "blob_packs", "blob_pack_entries", "photo_source_claims", "photo_timeline")
	copyStoreCatalogFixture(t, source, setup, tables)
	target.primaryStoreID = source.primaryStoreID
	var sidecars [2]Node
	for i, s := range []*Store{source, target} {
		var err error
		sidecars[i], err = s.CreateFile(ctx, s.RootID(), "capture.cr2.xmp", fakeHash("e5"), 4, "application/rdf+xml")
		require.NoError(t, err)
		targets, err := s.PhotoXMPTargetsAfter(ctx, "", 10)
		require.NoError(t, err)
		require.NotEmpty(t, targets, "a new sibling sidecar must requeue a settled original")
		var queued int
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM photo_xmp_unsettled WHERE asset_id=$1`, asset.ID).Scan(&queued))
		require.Equal(t, 1, queued)
	}
	// Moving the unowned sidecar away restores the saved occupancy token and
	// removes the queued work. This requires considering its OLD parent/name.
	for i, s := range []*Store{source, target} {
		_, _, err := s.Move(ctx, sidecars[i].ID, s.RootID(), "unpaired.xmp", sidecars[i].Revision)
		require.NoError(t, err)
		var queued int
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM photo_xmp_unsettled WHERE asset_id=$1`, asset.ID).Scan(&queued))
		require.Zero(t, queued)
	}
	// Renaming the original changes its saved photo input even though none of
	// its photo_files memberships change.
	for _, s := range []*Store{source, target} {
		current, err := s.NodeByID(ctx, raw.NodeID)
		require.NoError(t, err)
		_, _, err = s.Move(ctx, raw.NodeID, s.RootID(), "renamed.cr2", current.Revision)
		require.NoError(t, err)
		targets, err := s.PhotoXMPTargetsAfter(ctx, "", 10)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		require.Equal(t, raw.NodeID, targets[0].NodeID)
	}
}

func TestDocumentTextSuppressionPostgres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	source := newTestStore(t)
	hash := fakeHash("f2")
	node, err := source.CreateFile(ctx, source.RootID(), "purged.txt", hash, 4, "text/plain")
	require.NoError(t, err)
	_, err = source.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{node.CurrentVersionID}})
	require.NoError(t, err)
	setup, owner := pgtest.NewDatabase(t)
	target := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	tables := append(append([]string{}, postgresEnrollmentCatalogs...), "blob_locations")
	copyStoreCatalogFixture(t, source, setup, tables)
	target.primaryStoreID = source.primaryStoreID
	// Purge isn't ported yet. Its real SQLite output supplies the retained
	// suppression against which the shared scheduling query must operate.
	require.NoError(t, target.withStorageTx(ctx, func(tx *sql.Tx) error {
		return queueTextExtractionTx(ctx, tx, node.CurrentVersionID, hash, "text/plain")
	}))
	var queued, searchable int
	require.NoError(t, target.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM text_extraction_queue),
		(SELECT count(*) FROM text_searchable_versions)`).Scan(&queued, &searchable))
	require.Zero(t, queued)
	require.Zero(t, searchable)
	// Suppression belongs to the purged version, not all future uses of its bytes.
	created, err := target.CreateFile(ctx, target.RootID(), "new.txt", hash, 4, "text/plain")
	require.NoError(t, err)
	var version, queuedHash string
	require.NoError(t, target.db.QueryRowContext(ctx, `SELECT version_id FROM text_searchable_versions`).Scan(&version))
	require.Equal(t, created.CurrentVersionID, version)
	require.NoError(t, target.db.QueryRowContext(ctx, `SELECT blob_hash FROM text_extraction_queue`).Scan(&queuedHash))
	require.Equal(t, hash, queuedHash)
}

func TestDocumentBlobPromotionPostgres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	hash := fakeHash("c1")
	physical := BlobPhysical{Encoding: "zstd", StoredBytes: 4, PackEligible: true, Created: true, MD5: "11111111111111111111111111111111"}
	require.NoError(t, s.RecordBlob(ctx, hash, 20, physical))
	for _, table := range []string{"rendition_blob_staging", "derivative_blob_purge_pending"} {
		_, err := setup.ExecContext(ctx, "INSERT INTO documents."+table+"(blob_hash) VALUES($1)", hash)
		require.NoError(t, err)
	}
	receipt, err := s.CreateFileWithReceipt(ctx, s.RootID(), "promoted.txt", hash, 20, "text/plain", physical)
	require.NoError(t, err)
	require.Equal(t, physical.MD5, receipt.Node.MD5)
	require.Equal(t, PhysicalContent{Kind: "loose", Encoding: "zstd", LogicalBytes: 20, StoredBytes: 4, PackEligible: true}, receipt.Physical)
	var pending int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM rendition_blob_staging)+(SELECT count(*) FROM derivative_blob_purge_pending)`).Scan(&pending))
	require.Zero(t, pending, "a document reference cancels staged or pending derivative collection")
	_, err = s.ConfirmContentWithReceipt(ctx, receipt.Node.ID, receipt.Node.Revision, hash, 20, "text/plain", BlobPhysical{Encoding: "raw", StoredBytes: 20, PackEligible: true})
	require.ErrorContains(t, err, "verified repair required")
	preserved, err := s.PhysicalContent(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, receipt.Physical, preserved)
	// A retained historical version without physical authority cannot become head.
	replacement, err := s.ReplaceContentWithReceipt(ctx, receipt.Node.ID, receipt.Node.Revision, fakeHash("c2"), 8, "text/plain")
	require.NoError(t, err)
	_, err = setup.ExecContext(ctx, `DELETE FROM documents.blob_locations WHERE blob_hash=$1`, hash)
	require.NoError(t, err)
	refused, _, _, err := s.RevertContent(ctx, receipt.Node.ID, replacement.Node.Revision, receipt.Node.CurrentVersionID)
	require.ErrorIs(t, err, ErrPhysicalAuthorityMissing)
	require.Zero(t, refused.ID)
	current, err := s.NodeByID(ctx, receipt.Node.ID)
	require.NoError(t, err)
	require.Equal(t, replacement.Node, current)
}

func TestDocumentReintroducedPackMappingsPostgres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	hash := fakeHash("d1")
	// Logical collection may leave dead mappings in immutable packs. One is
	// primary, one belongs to a secondary: reintroduction removes only the first.
	const secondary = "11111111-1111-4111-8111-111111111111"
	packID := pack.NewPackID()
	_, err := setup.ExecContext(ctx, `INSERT INTO documents.blob_stores(store_id,name,kind,role,lifecycle,binding,ownership_epoch,created_at)
  VALUES($1,'archive','filesystem','secondary','active','synthetic','22222222-2222-4222-8222-222222222222',$2)`, secondary, nowRFC3339())
	require.NoError(t, err)
	for _, storeID := range []string{s.primaryStoreID, secondary} {
		_, err := setup.ExecContext(ctx, `INSERT INTO documents.blob_packs(store_id,pack_id,entry_count,stored_bytes,created_at) VALUES($1,$2,1,32,$3)`, storeID, packID, nowRFC3339())
		require.NoError(t, err)
		_, err = setup.ExecContext(ctx, `INSERT INTO documents.blob_pack_entries(blob_hash,store_id,pack_id,pack_offset,stored_len,raw_len,flags,crc32c) VALUES($1,$2,$3,$4,3,3,0,0)`, hash, storeID, packID, pack.MinEntryOffset)
		require.NoError(t, err)
	}
	_, err = s.CreateFile(ctx, s.RootID(), "reintroduced.bin", hash, 3, "application/octet-stream")
	require.NoError(t, err)
	for _, storeID := range []string{s.primaryStoreID, secondary} {
		var entries, stored, raw, maxStored, maxRaw int64
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT live_entries,live_stored_bytes,live_raw_bytes,max_live_stored_len,max_live_raw_len FROM blob_packs WHERE store_id=$1`, storeID).Scan(&entries, &stored, &raw, &maxStored, &maxRaw))
		expected := []int64{0, 0, 0, 0, 0}
		if storeID == secondary {
			expected = []int64{1, 3, 3, 3, 3}
		}
		require.Equal(t, expected, []int64{entries, stored, raw, maxStored, maxRaw})
	}
	var mappedStore string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT store_id FROM blob_pack_entries WHERE blob_hash=$1`, hash).Scan(&mappedStore))
	require.Equal(t, secondary, mappedStore)
}
