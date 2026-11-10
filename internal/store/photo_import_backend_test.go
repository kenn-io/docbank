package store

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkPhotoImportEmbeddedObservation(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	evidence, err := s.BeginIngest(ctx, "Embedded:agent", "Supplied evidence")
	require.NoError(t, err)
	node, err := s.IngestFileExact(ctx, evidence, s.RootID(), "capture.jpg", fakeHash("61"), 4, "image/jpeg", "/evidence/capture.jpg", "")
	require.NoError(t, err)
	run, err := s.BeginIngest(ctx, "photo-import", "/synthetic")
	require.NoError(t, err)
	member := photoImportTestMember("/synthetic/capture.jpg", PhotoRoleImage, node.BlobHash, "image/jpeg")
	first, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(member))
	require.NoError(t, err)
	require.Equal(t, node.ID, first.Nodes[0].ID)
	facts, err := s.NodeProvenance(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, facts.Items, 2)
	member.BlobHash = fakeHash("62")
	changed, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(member))
	require.NoError(t, err)
	require.Equal(t, node.ID, changed.Nodes[0].ID, "supplied evidence must not act as an independent imported source")
	require.Equal(t, member.BlobHash, changed.Nodes[0].BlobHash)
	checkRetentionMetadata(t, s)
}

func checkPhotoImportRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "photo-import", "/synthetic")
	require.NoError(t, err)
	group := photoImportTestGroup(
		photoImportTestMember("/synthetic/capture.jpg", PhotoRoleImage, fakeHash("71"), "image/jpeg"),
		photoImportTestMember("/synthetic/capture.jpg.xmp", PhotoRoleSidecar, fakeHash("72"), "application/rdf+xml"),
	)
	group.Members[0].OriginalMtime = "2026-01-02T03:04:05Z"
	var role string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	role = pgx.Identifier{role}.Sanitize()
	_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents.photo_change_receipts FROM "+role)
	require.NoError(t, err)
	_, err = s.IngestPhotoGroup(ctx, run, group)
	require.ErrorContains(t, err, "permission denied")
	for _, table := range []string{"content_versions", "blobs", "provenance", "photo_assets", "photo_files", "photo_source_claims", "photo_timeline", "photo_xmp_unsettled"} {
		var count int
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count))
		require.Zero(t, count, table)
	}
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON documents.photo_change_receipts TO "+role)
	require.NoError(t, err)
	result, err := s.IngestPhotoGroup(ctx, run, group)
	require.NoError(t, err)
	require.Equal(t, s.RootID()+1, result.Nodes[0].ID, "failed import must not consume node IDs")
	file := fileByRole(result.Asset.Files, PhotoRoleImage)
	var source, mtime, day string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT original_path,original_mtime FROM photo_source_claims WHERE file_id=$1`, file.ID).Scan(&source, &mtime))
	require.Equal(t, "/synthetic/capture.jpg", source)
	require.Equal(t, "2026-01-02T03:04:05Z", mtime)
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT day FROM photo_timeline WHERE file_id=$1`, file.ID).Scan(&day))
	require.Equal(t, "2026-01-02", day)
	var queued bool
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_xmp_unsettled WHERE asset_id=$1)`, result.Asset.ID).Scan(&queued))
	require.True(t, queued)
	publishXMPOrientation(t, s, file.NodeID, fakeHash("ee"))
	targets, err := s.PhotoXMPTargetsAfter(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, file.ID, targets[0].FileID)
	checkRetentionMetadata(t, s)
}

// The receipt is produced by SQLite's qualified XMP publisher. Both backends
// then perform native import against it; this does not qualify the publisher.
func checkPhotoImportAcceptedSidecarPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	source := newTestStore(t)
	ctx := t.Context()
	run, err := source.BeginIngest(ctx, "photo-import", "/synthetic")
	require.NoError(t, err)
	group := photoImportTestGroup(
		photoImportTestMember("/synthetic/capture.jpg", PhotoRoleImage, fakeHash("81"), "image/jpeg"),
		photoImportTestMember("/synthetic/capture.jpg.xmp", PhotoRoleSidecar, fakeHash("82"), "application/rdf+xml"),
	)
	first, err := source.IngestPhotoGroup(ctx, run, group)
	require.NoError(t, err)
	image := fileByRole(first.Asset.Files, PhotoRoleImage)
	publishXMPOrientation(t, source, image.NodeID, fakeHash("ee"))
	work, active, err := source.ClassifyPhotoXMP(ctx, image.ID)
	require.NoError(t, err)
	require.True(t, active)
	plan, conflicts := planXMP(work, PhotoXMPPacket{Keywords: []string{}, Present: PhotoXMPKeywordsBit}, "")
	require.Zero(t, conflicts)
	plan.Write = true
	_, err = source.PublishPhotoXMP(ctx, plan, fakeHash("83"), 8, BlobPhysical{Encoding: "raw", StoredBytes: 8})
	require.NoError(t, err)
	checkRetentionMetadata(t, source)
	target, _ := copyRetentionStore(t, source)
	for _, s := range []*Store{source, target} {
		for _, path := range []string{"/synthetic/capture.jpg.xmp", "/another/capture.jpg.xmp"} {
			group.Members[0].OriginalPath = strings.TrimSuffix(path, ".xmp")
			group.Members[1].OriginalPath = path
			result, err := s.IngestPhotoGroup(ctx, run, group)
			require.NoError(t, err)
			require.True(t, result.Skipped)
			require.Equal(t, first.Nodes[1].ID, result.Nodes[1].ID)
			require.Equal(t, fakeHash("83"), result.Nodes[1].BlobHash, "accepted outside bytes must not replace the published sidecar")
		}
		checkRetentionMetadata(t, s)
		// Inspect the production correlated lookup, not an independently written
		// expression. SQLite must seek the receipt index; PostgreSQL must match it.
		err = s.withReadTx(ctx, func(tx *sql.Tx) error {
			prefix := "EXPLAIN QUERY PLAN "
			if s.postgresSchema != "" {
				_, err := tx.ExecContext(ctx, "SET LOCAL enable_seqscan=off")
				if err != nil {
					return err
				}
				prefix = "EXPLAIN (COSTS OFF) "
			}
			rows, err := tx.QueryContext(ctx, prefix+s.photoImportSidecarCandidatesSQL(), fakeHash("82"), "/synthetic/capture.jpg.xmp", "/synthetic/capture.jpg.xmp")
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
			queryPlan := strings.Join(lines, "\n")
			if s.postgresSchema == "" {
				require.Contains(t, queryPlan, "SEARCH xr USING INDEX photo_change_receipts_xmp_file")
			} else {
				require.Regexp(t, `(?m)Index Scan using photo_change_receipts_xmp_(file|sidecar)[^\n]*\n[^\n]*Index Cond:`, queryPlan)
			}
			return rows.Err()
		})
		require.NoError(t, err)
	}
	t.Run("restored null outside list", func(t *testing.T) {
		// Metadata accepts an explicit JSON null for this optional list and
		// retains the receipt bytes. Exercise the real restore before import.
		_, err := source.db.ExecContext(ctx, `UPDATE photo_change_receipts
			SET after_json=json_set(after_json,'$.sync.outside',json('null'))
			WHERE operation='xmp_sync'`)
		require.NoError(t, err)
		restored := restoredXMPStore(t, source)
		postgres, _ := copyRetentionStore(t, restored)
		group.Members[1].BlobHash = fakeHash("83")
		group.Members[1].Size = 8
		for _, s := range []*Store{restored, postgres} {
			result, err := s.IngestPhotoGroup(t.Context(), run, group)
			require.NoError(t, err)
			require.True(t, result.Skipped)
			require.Equal(t, first.Nodes[1].ID, result.Nodes[1].ID)
			require.Equal(t, fakeHash("83"), result.Nodes[1].BlobHash)
		}
	})
}
