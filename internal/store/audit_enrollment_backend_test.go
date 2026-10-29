package store

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func TestAuditEnrollmentPhotosPostgres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	type photoInputSnapshot struct {
		FileID, Token                                      string
		Eligible, Extracted, Pending, Audited, AuditActive bool
		Conflicts                                          int64
	}

	readInputs := func(s *Store) []photoInputSnapshot {
		out := []photoInputSnapshot{}
		require.NoError(t, queryRows(t.Context(), s.db, `SELECT file_id,token,eligible,extracted,pending,audited,audit_active,conflicts FROM photo_xmp_inputs ORDER BY file_id`, nil, func(rows *sql.Rows) error {
			var row photoInputSnapshot
			if err := rows.Scan(&row.FileID, &row.Token, &row.Eligible, &row.Extracted, &row.Pending, &row.Audited, &row.AuditActive, &row.Conflicts); err != nil {
				return err
			}
			out = append(out, row)
			return nil
		}))
		return out
	}

	readQueue := func(s *Store) []string {
		out := []string{}
		require.NoError(t, queryRows(t.Context(), s.db, `SELECT asset_id FROM photo_xmp_unsettled ORDER BY asset_id`, nil, func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
			return nil
		}))
		return out
	}

	setup, owner := pgtest.NewDatabase(t)
	destination := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	source, asset, raw := xmpSidecarPhoto(t)
	// Both backends represent the same vault, including canonical record hashes.
	source.vaultID = destination.vaultID
	_, err := source.writeDB.ExecContext(ctx, `UPDATE vault_metadata SET vault_uid=$1`, source.vaultID)
	require.NoError(t, err)
	scope, err := source.Mkdir(ctx, source.RootID(), "Audited")
	require.NoError(t, err)
	sidecars, err := source.Mkdir(ctx, source.RootID(), "Sidecars")
	require.NoError(t, err)
	originals, err := source.Mkdir(ctx, source.RootID(), "Originals")
	require.NoError(t, err)
	for _, file := range asset.Files {
		parent := originals.ID
		if file.Role == PhotoRoleSidecar {
			parent = sidecars.ID
		}
		node := requireNodeByID(t, source, file.NodeID)
		_, _, err = source.Move(ctx, node.ID, parent, node.Name, node.Revision)
		require.NoError(t, err)
	}
	importXMP(t, source, raw.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating})
	// These originals distinguish eligibility from extraction and live status.
	for i, name := range []string{"pending.jpg", "unextracted.jpg", "trashed.jpg", "untouched.jpg", "movie.mp4", "tagged.jpg"} {
		mime := "image/jpeg"
		if name == "movie.mp4" {
			mime = "video/mp4"
		}
		node, err := source.CreateFile(ctx, source.RootID(), name, fakeHash([]string{"a2", "a3", "a4", "a5", "a6", "a7"}[i]), 4, mime)
		require.NoError(t, err)
		photo, err := source.PhotoAssetForNode(ctx, node.ID)
		require.NoError(t, err)
		if name != "untouched.jpg" && name != "tagged.jpg" {
			_, err = editPhotoAuthored(ctx, source, []PhotoAuthoredTarget{{FileID: photo.Files[0].ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(3)}}})
			require.NoError(t, err)
		}
		if name == "tagged.jpg" {
			tag, err := source.CreateTag(ctx, "favorite")
			require.NoError(t, err)
			_, err = source.AssignTag(ctx, tag.ID, node.ID, node.Revision)
			require.NoError(t, err)
		}
		if name != "unextracted.jpg" {
			publishXMPOrientation(t, source, node.ID, fakeHash("ee"))
		}
		if name == "trashed.jpg" {
			_, _, err = source.Trash(ctx, node.ID, UnconditionalRev)
			require.NoError(t, err)
		}
	}
	for _, target := range []int64{scope.ID, sidecars.ID, originals.ID} {
		// Classification after the first enrollment settles the global audit-state
		// change. Enrolling just the sidecar must requeue its original's asset.
		if target != scope.ID {
			_, _, err = source.ClassifyPhotoXMP(ctx, raw.ID)
			require.NoError(t, err)
		}
		copyStoreCatalogFixture(t, source, setup, []string{
			"nodes", "blobs", "blob_checksums", "content_versions", "ingests", "provenance",
			"source_metadata_generations", "source_metadata_heads", "photo_technical_metadata",
			"tags", "node_tags", "photo_assets", "photo_files", "photo_sets", "photo_change_receipts",
			"audit_records", "audit_authority", "audit_scopes", "audit_baselines", "audit_memberships", "photo_xmp_unsettled",
		})
		before, err := destination.AuditStatus(ctx, nil)
		require.NoError(t, err)
		beforeRecords, err := loadInitialAuditRecords(ctx, destination.db)
		require.NoError(t, err)
		beforeInputs := readInputs(destination)
		require.Equal(t, readInputs(source), beforeInputs)
		beforeQueue := readQueue(destination)
		if target != scope.ID {
			require.NotContains(t, beforeQueue, asset.ID, "the previous receipt has settled before membership changes")
		}
		plan, err := source.PreviewInitialAudit(ctx, target, "cli", nil)
		require.NoError(t, err)
		var initial initialAuditEnrollmentSet
		var additional additionalAuditEnrollmentSet
		require.NoError(t, source.withStorageTx(ctx, func(tx *sql.Tx) error {
			var err error
			if plan.initial {
				initial, err = source.buildInitialAuditEnrollment(ctx, tx, plan.input)
			} else {
				additional, err = source.buildAdditionalAuditEnrollment(ctx, tx, plan.input)
			}
			return err
		}))
		_, err = source.EnableInitialAudit(ctx, plan)
		require.NoError(t, err)
		require.NoError(t, source.ValidateMetadata(ctx))
		want, err := source.AuditStatus(ctx, nil)
		require.NoError(t, err)
		wantRecords, err := loadInitialAuditRecords(ctx, source.db)
		require.NoError(t, err)
		discard := errors.New("discard enrollment")
		for _, commit := range []bool{false, true} {
			err := destination.withStorageTx(ctx, func(tx *sql.Tx) error {
				var err error
				if plan.initial {
					err = destination.persistInitialAuditEnrollment(ctx, tx, initial)
				} else {
					err = destination.persistAdditionalAuditEnrollment(ctx, tx, additional)
				}
				if err != nil {
					return err
				}
				if !commit {
					return discard
				}
				return nil
			})
			expected, expectedRecords := before, beforeRecords
			inputs, queue := beforeInputs, beforeQueue
			if commit {
				require.NoError(t, err)
				expected, expectedRecords = want, wantRecords
				inputs, queue = readInputs(source), readQueue(source)
				require.Contains(t, queue, asset.ID, "enrolling a sidecar also queues the asset's original")
			} else {
				require.ErrorIs(t, err, discard)
			}
			got, err := destination.AuditStatus(ctx, nil)
			require.NoError(t, err)
			require.Equal(t, expected, got)
			records, err := loadInitialAuditRecords(ctx, destination.db)
			require.NoError(t, err)
			require.Len(t, records, len(expectedRecords))
			for kind, group := range expectedRecords {
				require.ElementsMatch(t, group, records[kind], kind)
			}
			require.Equal(t, inputs, readInputs(destination))
			require.Equal(t, queue, readQueue(destination))
		}
		// Exercise the production cursor across one-item pages and the terminal page.
		after := ""
		for {
			want, err := source.PhotoXMPTargetsAfter(ctx, after, 1)
			require.NoError(t, err)
			got, err := destination.PhotoXMPTargetsAfter(ctx, after, 1)
			require.NoError(t, err)
			require.Equal(t, want, got)
			if len(want) == 0 {
				break
			}
			after = want[0].Cursor()
		}
	}
}
