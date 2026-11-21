package store

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func checkBrowserPhotoRecovery(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	ctx := t.Context()
	const source = "20000000-0000-4000-8000-000000000001"
	_, found, err := s.BrowserPhotoImport(ctx, "")
	require.NoError(t, err)
	require.False(t, found)
	run, err := s.BeginIngest(ctx, "photo-import", "browser: Card")
	require.NoError(t, err)
	image := photoImportTestMember("browser:"+source+"/Card/first.JPG", PhotoRoleImage, fakeHash("Card/first.JPG"), "image/jpeg")
	sidecar := photoImportTestMember("browser:"+source+"/Card/first.XMP", PhotoRoleSidecar, fakeHash("Card/first.XMP"), "application/rdf+xml")
	_, err = s.IngestPhotoGroup(ctx, run, photoImportTestGroup(image, sidecar))
	require.NoError(t, err)
	other := photoImportTestMember("browser:"+source+"/Card/other.XMP", PhotoRoleSidecar, fakeHash("Card/other.XMP"), "application/rdf+xml")
	_, err = s.IngestPhotoGroup(ctx, run, photoImportTestGroup(other))
	require.NoError(t, err)
	result, found, err := s.BrowserPhotoImport(ctx, source)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "Card", result.Folder)
	require.True(t, result.FolderSelection)
	require.Equal(t, 2, result.Groups)
	require.Equal(t, fakeHash("Card/other.XMP"), result.Members["Card/other.XMP"])
	paths, err := s.BrowserPhotoImportPaths(ctx, []string{"other.XMP"})
	require.NoError(t, err)
	require.Contains(t, paths, BrowserPhotoImportMember{
		SourceID: source, Folder: "Card", FolderSelection: true,
		RelativePath: "Card/other.XMP", BlobHash: fakeHash("Card/other.XMP"),
	})
	node, err := s.NodeByPath(ctx, "/other.XMP")
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)
	result, found, err = s.BrowserPhotoImport(ctx, source)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 2, result.Groups)
	require.Equal(t, fakeHash("Card/other.XMP"), result.Members["Card/other.XMP"])
	looseRun, err := s.BeginIngest(ctx, "photo-import", "browser: Choose files")
	require.NoError(t, err)
	const looseID = "20000000-0000-4000-8000-000000000002"
	loose := photoImportTestMember("browser:"+looseID+"/loose.XMP", "", fakeHash("loose"), "")
	_, err = s.IngestPhotoGroup(ctx, looseRun, photoImportTestGroup(loose))
	require.NoError(t, err)
	looseResult, found, err := s.BrowserPhotoImport(ctx, looseID)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, looseResult.FolderSelection)
	require.Equal(t, 1, looseResult.Groups)
	paths, err = s.BrowserPhotoImportPaths(ctx, []string{"loose.XMP"})
	require.NoError(t, err)
	require.Contains(t, paths, BrowserPhotoImportMember{
		SourceID: looseID, Folder: "Choose files", FolderSelection: false,
		RelativePath: "loose.XMP", BlobHash: fakeHash("loose"),
	})
	latest, found, err := s.BrowserPhotoImport(ctx, "")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, looseID, latest.SourceID)
	conflicting := photoImportTestMember("browser:"+looseID+"/loose.XMP", PhotoRoleSidecar, fakeHash("changed"), "application/rdf+xml")
	group := photoImportTestGroup(conflicting)
	group.CreateOnly = true
	_, err = s.IngestPhotoGroup(ctx, looseRun, group)
	require.NoError(t, err)
	looseResult, found, err = s.BrowserPhotoImport(ctx, looseID)
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, looseResult.Members["loose.XMP"])
	invalid := photoImportTestMember("browser:not-a-uuid/invalid.XMP", PhotoRoleSidecar, fakeHash("invalid"), "application/rdf+xml")
	_, err = s.IngestPhotoGroup(ctx, looseRun, photoImportTestGroup(invalid))
	require.NoError(t, err)
	paths, err = s.BrowserPhotoImportPaths(ctx, []string{"invalid.XMP"})
	require.NoError(t, err)
	require.Empty(t, paths)
}

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
	setup, owner, login := newTemplateDatabase(t)
	s := openPostgresStore(t, owner, login)
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
			rows, err := tx.QueryContext(ctx, prefix+s.photoImportSidecarCandidatesSQL(), fakeHash("82"), "/synthetic/capture.jpg.xmp", storedString(s.postgresSchema != "", "/synthetic/capture.jpg.xmp"))
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
