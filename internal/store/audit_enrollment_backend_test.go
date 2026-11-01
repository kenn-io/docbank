package store

import (
	"database/sql"
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
	run, err := source.BeginIngest(ctx, "filesystem", "Synthetic audit fixture")
	require.NoError(t, err)
	report, _, err := source.IngestFileWithMembership(ctx, run, scope.ID, "report.txt", fakeHash("b8"), 4, "text/plain", "/synthetic/report.txt", "")
	require.NoError(t, err)
	_, err = source.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{report.CurrentVersionID}})
	require.NoError(t, err)
	_, _, err = source.Trash(ctx, report.ID, UnconditionalRev)
	require.NoError(t, err)
	_, err = source.SetPhotoSettings(ctx, 1, PhotoSettingsUpdate{BurstGapSeconds: new(3)})
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
		copyStoreCatalogFixture(t, source, setup, postgresEnrollmentCatalogs)
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
		path, err := source.Path(ctx, target)
		require.NoError(t, err)
		plan, err := destination.PreviewInitialAuditPath(ctx, path, "cli", nil)
		require.NoError(t, err)
		_, err = source.EnableInitialAudit(ctx, plan)
		require.NoError(t, err)
		require.NoError(t, source.ValidateMetadata(ctx))
		want, err := source.AuditStatus(ctx, nil)
		require.NoError(t, err)
		wantRecords, err := loadInitialAuditRecords(ctx, source.db)
		require.NoError(t, err)
		for _, invalid := range []bool{true, false} {
			if invalid {
				// Corrupt an unrelated document after preview. Full validation
				// must reject enrollment and roll back its records and queue.
				_, err = setup.ExecContext(ctx, `UPDATE documents.blobs SET size=5 WHERE hash=$1`, fakeHash("a2"))
				require.NoError(t, err)
			}
			_, err := destination.EnableInitialAudit(ctx, plan)
			expected, expectedRecords := want, wantRecords
			inputs, queue := readInputs(source), readQueue(source)
			if invalid {
				require.ErrorContains(t, err, "content version size differs from blob authority")
				expected, expectedRecords = before, beforeRecords
				inputs, queue = beforeInputs, beforeQueue
			} else {
				require.NoError(t, err)
				require.Contains(t, queue, asset.ID, "enrolling a sidecar also queues the asset's original")
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
			if invalid {
				_, err = setup.ExecContext(ctx, `UPDATE documents.blobs SET size=4 WHERE hash=$1`, fakeHash("a2"))
				require.NoError(t, err)
			}
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
		_, err = destination.PreviewInitialAudit(ctx, target, "cli", nil)
		require.ErrorIs(t, err, ErrAuditScopeOverlap)
	}
	// A well-formed projection can still disagree with canonical history.
	// Additional preview must validate that history before proposing a scope.
	_, err = setup.ExecContext(ctx, `UPDATE documents.audit_baselines SET target_node_id=$1 WHERE target_node_id=$2`, source.RootID(), scope.ID)
	require.NoError(t, err)
	_, err = destination.PreviewInitialAudit(ctx, source.RootID(), "cli", nil)
	require.ErrorContains(t, err, "audit baseline relation does not match its scope and operation")
}

// Copy retained catalogs in dependency order; current-version, cover and key-face links are deferred.
var postgresEnrollmentCatalogs = []string{
	"audit_records",
	"batch_tag_receipts",
	"bates_namespaces",
	"blob_stores",
	"blobs",
	"collection_snapshots",
	"current_rendition_roots",
	"derivative_purge_suppressions",
	"document_people_builds",
	"document_people_state",
	"email_generations",
	"embedding_vector_spaces",
	"export_sources",
	"extracted_text",
	"ingests",
	"mailbox_archives",
	"mailbox_containers",
	"media_sources",
	"nodes",
	"package_preflights",
	"page_recipes",
	"person_aliases",
	"person_external_uid_aliases",
	"person_match_candidates",
	"person_merges",
	"person_splits",
	"persons",
	"photo_assets",
	"photo_checkouts",
	"photo_hidden_credentials",
	"photo_hidden_failures",
	"photo_hidden_lockout",
	"photo_library_settings",
	"photo_xmp_unsettled",
	"processing_incarnations",
	"processing_profiles",
	"rendition_lexical_generations",
	"saved_queries",
	"tags",
	"term_report_history",
	"audit_authority",
	"audit_scopes",
	"bates_allocations",
	"bates_namespace_cursors",
	"blob_checksums",
	"collection_labels",
	"content_versions",
	"current_processing_incarnation",
	"email_part_artifacts",
	"embedding_input_generations",
	"embedding_vector_sets",
	"export_chunks",
	"export_plans",
	"mailbox_chunks",
	"mailbox_jobs",
	"media_operations",
	"node_tags",
	"packages",
	"person_candidate_faces",
	"person_external_identities",
	"person_identities",
	"photo_checkout_entries",
	"photo_sets",
	"processing_consent_grants",
	"processing_consent_revocations",
	"provenance",
	"rendition_builds",
	"rendition_jobs",
	"rendition_lexical_generation_manifests",
	"rendition_lexical_heads",
	"rendition_lexical_superseded",
	"saved_query_runs",
	"source_metadata_generations",
	"storage_operations",
	"watch_sources",
	"audit_baselines",
	"bates_artifacts",
	"bates_page_labels",
	"collection_snapshot_members",
	"custodian_assignments",
	"document_people",
	"document_people_generations",
	"document_people_heads",
	"email_attachments",
	"embedding_failures",
	"embedding_generation_inputs",
	"embedding_jobs",
	"embedding_sets",
	"embedding_vector_rows",
	"export_documents",
	"export_jobs",
	"export_members",
	"export_role_roots",
	"media_source_versions",
	"package_import_jobs",
	"package_import_receipts",
	"package_labels",
	"package_records",
	"package_volumes",
	"page_documents",
	"page_render_jobs",
	"person_document_assertions",
	"photo_change_receipts",
	"photo_files",
	"photo_quality_signals",
	"photo_set_members",
	"photo_suggestions",
	"photo_technical_metadata",
	"provenance_version_bindings",
	"push_sources",
	"rendition_artifacts",
	"rendition_attachments",
	"rendition_job_waiters",
	"rendition_lexical_generation_builds",
	"rendition_lexical_index",
	"rendition_units",
	"source_metadata_heads",
	"visual_preview_generations",
	"audit_memberships",
	"bates_artifact_pages",
	"collection_snapshot_representations",
	"email_body_results",
	"email_document_publications",
	"email_heads",
	"embedding_heads",
	"media_occurrences",
	"page_frames",
	"photo_faces",
	"rendition_heads",
	"rendition_lexical_segments",
	"visual_preview_heads",
	"email_document_relations",
	"mailbox_transfer_receipts",
	"media_input_artifacts",
	"page_images",
	"mailbox_occurrences",
}
