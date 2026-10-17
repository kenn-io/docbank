package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPhotoEventsCaptureAssetAlbumAndRefetchChanges(t *testing.T) {
	t.Parallel()
	for _, rule := range []string{"asset exclusion", "album membership", "tags", "tags/create", "tags/delete", "provenance", "metadata and indexing", "extraction", "directory refetch", "collection coverage", "visual previews", "global settings and Hidden credentials refetch"} {
		t.Run(rule, func(t *testing.T) {
			ctx := t.Context()
			if rule == "collection coverage" {
				s, run, docs, profile := collectionCoverageFixture(t, 1)
				node, _, err := s.IngestFile(ctx, run, s.RootID(), "photo.jpg", fakeHash("11"), 4, "image/jpeg", "/synthetic/photo.jpg", "")
				require.NoError(t, err)
				events, cancel := s.SubscribePhotoEvents(ctx)
				t.Cleanup(cancel)
				collectionCoveragePublish(t, s, docs[0], profile, "complete")
				noPhotoEvent(t, events)
				collectionCoveragePublish(t, s, node, profile, "complete")
				require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
				noPhotoEvent(t, events)

				return
			}
			s := newTestStore(t)
			if rule == "global settings and Hidden credentials refetch" {
				events, cancel := s.SubscribePhotoEvents(ctx)
				t.Cleanup(cancel)
				require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
				require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
				_, err := s.SetPhotoSettings(t.Context(), 1, new("image"))
				require.NoError(t, err)
				require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
				token, _, err := s.UnlockPhotoHidden(t.Context(), "synthetic-passcode")
				require.NoError(t, err)
				require.NoError(t, s.LockPhotoHidden(WithPhotoHiddenToken(t.Context(), token)))
				require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
				noPhotoEvent(t, events)
				return
			}
			run, err := s.BeginIngest(ctx, "cli", "synthetic")
			require.NoError(t, err)
			dir, err := s.Mkdir(ctx, s.RootID(), "camera")
			require.NoError(t, err)
			node, _, err := s.IngestFile(ctx, run, dir.ID, "photo.jpg", fakeHash("11"), 4, "image/jpeg", "/synthetic/photo.jpg", "")
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, err)
			tag, err := s.CreateTag(ctx, "trip")
			require.NoError(t, err)
			events, cancel := s.SubscribePhotoEvents(t.Context())
			t.Cleanup(cancel)

			switch rule {
			case "asset exclusion":
				_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
				require.NoError(t, err)
				require.Equal(t, PhotoEvent{AssetID: asset.ID}, nextPhotoEvent(t, events))

			case "album membership":
				album, err := s.CreatePhotoSet(ctx, "Trip album")
				require.NoError(t, err)
				require.Equal(t, PhotoEvent{AlbumID: album.ID}, nextPhotoEvent(t, events))
				_, err = s.ChangePhotoSetMembers(ctx, album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: []string{asset.ID}})
				require.NoError(t, err)
				require.Equal(t, album.ID, nextPhotoEvent(t, events).AlbumID)

			case "tags/create", "tags/delete":
				if rule == "tags/create" {
					_, err = s.CreateTag(ctx, "New keyword")
				} else {
					_, err = s.DeleteTag(ctx, tag.ID, tag.Revision)
				}
				require.NoError(t, err)
				// Publication finishes with the write; close so a missing event fails immediately.
				cancel()
				require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))

			case "tags":
				_, err = s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
				require.NoError(t, err)
				require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
				noPhotoEvent(t, events)
				_, err = s.RenameTag(ctx, tag.ID, tag.Revision+1, "holiday")
				require.NoError(t, err)
				require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
				noPhotoEvent(t, events)
				require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `DELETE FROM node_tags WHERE node_id=?`, node.ID)
					return err
				}))
				require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
				noPhotoEvent(t, events)

			case "provenance":
				require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `INSERT INTO provenance(identity,node_id,ingest_id,original_path) VALUES('synthetic-membership',?,?,'/synthetic/another.jpg')`, node.ID, run.ID())
					return err
				}))
				require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))

			case "metadata and indexing":
				metadata := document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{{Key: "image.exif.iso", Namespace: "image.exif", SourceField: "ISO", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataInteger, Integer: new(int64(200))}}}}
				canonical, _, err := document.MarshalSourceMetadataV1(metadata)
				require.NoError(t, err)
				generation, err := s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("f1"), canonical)
				require.NoError(t, err)
				require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
				noPhotoEvent(t, events)
				fingerprints, err := document.CurrentPhotoQualityFingerprints()
				require.NoError(t, err)
				for _, statement := range []string{
					`UPDATE photo_files SET rating=3 WHERE node_id=?`,
					`UPDATE photo_technical_metadata SET iso=400 WHERE generation_id=?`,
					fmt.Sprintf(`INSERT INTO photo_quality_signals(content_version_id,evaluator_fingerprint,state) VALUES(?,'%s','unavailable')`, fingerprints.Evaluator),
					`INSERT INTO text_searchable_versions(version_id) VALUES(?)`,
				} {
					var value any = node.CurrentVersionID
					switch {
					case strings.Contains(statement, "node_id=?"):
						value = node.ID
					case strings.HasPrefix(statement, "UPDATE photo_technical_metadata"):
						value = generation.GenerationID
					}
					require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, statement, value); return err }), statement)
					want := PhotoEvent{Refetch: true}
					if strings.Contains(statement, "node_id=?") {
						want = PhotoEvent{AssetID: asset.ID}
					}
					require.Equal(t, want, nextPhotoEvent(t, events), statement)
					noPhotoEvent(t, events)
				}

			case "extraction":
				require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: node.BlobHash, Extractor: "synthetic", ExtractorVersion: 1, Status: ExtractionOK, Text: "Trip"}))
				require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))

			case "directory refetch":
				dir, err = s.NodeByID(ctx, dir.ID)
				require.NoError(t, err)
				_, _, err = s.Move(ctx, dir.ID, s.RootID(), "renamed-camera", dir.Revision)
				require.NoError(t, err)
				require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
				noPhotoEvent(t, events)

			case "visual previews":
				canonical := readyVisualPreview(t, node.BlobHash, fakeHash("22"), 9)
				physical := &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9}
				_, err = s.PublishVisualPreview(ctx, node.CurrentVersionID, canonical, physical)
				require.NoError(t, err)
				require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
				_, err = s.PublishVisualPreview(ctx, node.CurrentVersionID, canonical, physical)
				require.NoError(t, err)
				noPhotoEvent(t, events)
				recipe := visualPreviewRecipe()
				recipe.ProcessorFingerprint = fakeHash("33")
				another := readyVisualPreviewWithRecipe(t, node.BlobHash, fakeHash("44"), 9, recipe)
				_, err = s.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, another, physical)
				require.NoError(t, err)
				require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
			}
		})
	}
}

func TestPhotoEventsNonPhotoSharedBlobMembership(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "first.jpg", fakeHash("11"), 4, "image/jpeg")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "second.jpg", fakeHash("22"), 4, "image/jpeg")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "synthetic")
	require.NoError(t, err)
	events, cancel := s.SubscribePhotoEvents(ctx)
	defer cancel()
	baseline, err := s.CreateFile(ctx, s.RootID(), "note.txt", fakeHash("33"), 4, "text/plain")
	require.NoError(t, err)
	noPhotoEvent(t, events)
	baseline, _, err = s.ReplaceContent(ctx, baseline.ID, baseline.Revision, fakeHash("44"), 4, "text/plain")
	require.NoError(t, err)
	noPhotoEvent(t, events)
	baseline, _, err = s.Trash(ctx, baseline.ID, baseline.Revision)
	require.NoError(t, err)
	noPhotoEvent(t, events)
	_, _, err = s.Restore(ctx, baseline.ID, baseline.Revision)
	require.NoError(t, err)
	noPhotoEvent(t, events)
	doc, err := s.CreateFile(ctx, s.RootID(), "duplicate.bin", first.BlobHash, 4, "application/octet-stream")
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
	doc, _, err = s.Move(ctx, doc.ID, s.RootID(), "renamed.bin", doc.Revision)
	require.NoError(t, err)
	noPhotoEvent(t, events)
	_, err = s.AssignTag(ctx, tag.ID, doc.ID, doc.Revision)
	require.NoError(t, err)
	noPhotoEvent(t, events)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE nodes SET current_version_id=current_version_id,trashed_at=trashed_at,modified_at=? WHERE id=?`, nowRFC3339(), doc.ID)
		return err
	}))
	noPhotoEvent(t, events)
	doc, err = s.NodeByID(ctx, doc.ID)
	require.NoError(t, err)
	doc, _, err = s.Trash(ctx, doc.ID, doc.Revision)
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
	doc, _, err = s.Restore(ctx, doc.ID, doc.Revision)
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
	doc, _, err = s.ReplaceContent(ctx, doc.ID, doc.Revision, second.BlobHash, 4, "application/octet-stream")
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id=?`, doc.ID)
		return err
	}))
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
	noPhotoEvent(t, events)
}

func TestPhotoEventsCaptureDepictedPeople(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "portrait.jpg", fakeHash("11"), 4, "image/jpeg")
	require.NoError(t, err)
	person, err := s.CreatePerson(ctx, "Person A", "operator")
	require.NoError(t, err)
	events, cancel := s.SubscribePhotoEvents(ctx)
	t.Cleanup(cancel)
	assertion, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
	noPhotoEvent(t, events)
	for _, statement := range []string{
		`UPDATE person_document_assertions SET action='withdrawn',revision=revision+1 WHERE assertion_id=?`,
		`DELETE FROM person_document_assertions WHERE assertion_id=?`,
	} {
		require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, statement, assertion.AssertionID)
			return err
		}))
		require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
		noPhotoEvent(t, events)
	}
}

func TestPhotoEventsRefetchWhenPersonSearchesResolveDifferently(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ada, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	events, cancel := s.SubscribePhotoEvents(ctx)
	t.Cleanup(cancel)
	refetch := PhotoEvent{Refetch: true}
	_, err = s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	noPhotoEvent(t, events)
	_, err = s.CreatePerson(ctx, "ada", "operator")
	require.NoError(t, err)
	require.Equal(t, refetch, nextPhotoEvent(t, events))
	noPhotoEvent(t, events)
	ada, err = s.UpdatePerson(ctx, ada.PersonID, ada.Revision, "Ada Lovelace")
	require.NoError(t, err)
	require.Equal(t, refetch, nextPhotoEvent(t, events))
	noPhotoEvent(t, events)
	_, err = s.RetirePerson(ctx, ada.PersonID, ada.Revision)
	require.NoError(t, err)
	require.Equal(t, refetch, nextPhotoEvent(t, events))
	noPhotoEvent(t, events)
}

// Keep the schema inventory explicit: a new table needs a capture rule or a reason.
func TestPhotoEventSchemaCoverage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	exclusions := map[string]string{}
	for table := range strings.FieldsSeq("export_sources export_chunks export_members export_plans export_documents export_role_roots export_jobs collection_snapshots collection_snapshot_members collection_snapshot_representations packages package_volumes bates_namespaces bates_namespace_cursors bates_allocations bates_page_labels bates_artifacts bates_artifact_pages package_records package_labels package_import_jobs package_import_receipts page_documents page_frames page_recipes page_images page_render_jobs vault_metadata package_preflights document_event_state document_event_generations document_event_heads document_events document_event_actors document_event_primaries document_event_builds provenance_version_bindings document_event_dirty document_event_attempts media_sources media_source_versions media_occurrences media_input_artifacts media_operations ingests watch_sources push_sources current_processing_incarnation saved_query_runs term_report_history audit_records audit_authority audit_scopes audit_baselines audit_memberships email_generations email_part_artifacts email_attachments email_heads email_body_results email_document_publications email_document_relations mailbox_containers mailbox_chunks mailbox_archives mailbox_transfer_receipts mailbox_jobs mailbox_occurrences") {
		exclusions[table] = "Document, ingest, export, or audit bookkeeping publishes photo changes through captured nodes or provenance"
	}
	for table := range strings.FieldsSeq("blobs blob_checksums blob_stores blob_locations blob_packs blob_pack_entries storage_operations storage_operation_stores storage_operation_cleanup gc_loose_retirements") {
		exclusions[table] = "Physical storage bookkeeping does not change photo metadata"
	}
	for table := range strings.FieldsSeq("source_metadata_generations content_versions rendition_attachments") {
		exclusions[table] = "Immutable records become visible through captured nodes or heads"
	}
	for table := range strings.FieldsSeq("photo_technical_metadata_state") {
		exclusions[table] = "Metadata bookkeeping publishes through source_metadata_heads"
	}
	for table := range strings.FieldsSeq("processing_incarnations processing_consent_grants processing_consent_revocations processing_profiles rendition_builds rendition_artifacts rendition_units rendition_lexical_segments embedding_vector_spaces embedding_input_generations embedding_jobs embedding_generation_inputs embedding_vector_sets embedding_vector_rows embedding_sets embedding_heads embedding_failures rendition_lexical_index rendition_lexical_generations rendition_lexical_generation_manifests rendition_lexical_generation_builds rendition_lexical_heads rendition_lexical_superseded vector_index_generations vector_index_heads vector_index_build_jobs vector_index_reader_leases vector_index_unavailable_coverage rendition_jobs rendition_job_waiters current_rendition_roots derivative_purge_suppressions rendition_blob_staging derivative_blob_purge_pending derivative_pack_purge_pending text_extraction_queue") {
		exclusions[table] = "Processing bookkeeping publishes photo changes through captured rendition heads or searchable versions"
	}
	for table := range strings.FieldsSeq("batch_tag_receipts photo_change_receipts") {
		exclusions[table] = "Undo receipts do not change photo results"
	}
	for table := range strings.FieldsSeq("photo_timeline photo_timeline_pending photo_sidecar_considered") {
		exclusions[table] = "Derived photo state publishes through captured file or node changes"
	}
	for table := range strings.FieldsSeq("persons person_identities person_external_identities person_external_uid_aliases person_merges custodian_assignments photo_faces person_match_candidates document_people_state document_people_builds document_people_generations document_people_heads document_people person_splits person_candidate_faces") {
		exclusions[table] = "People bookkeeping publishes photo membership through person_document_assertions or person_aliases"
	}
	for table := range strings.FieldsSeq("photo_hidden_lockout photo_hidden_failures") {
		exclusions[table] = "Unlock throttling does not change photo visibility"
	}
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		require.NoError(t, installPhotoChangeTriggers(t.Context(), tx))
		rows, err := tx.QueryContext(t.Context(), `SELECT name FROM main.sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var tables []string
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			tables = append(tables, name)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		for _, table := range tables {
			// Search indexes and their SQLite shadow tables derive from captured nodes or heads.
			for _, index := range []string{"photo_metadata_fts", "rendition_lexical_fts", "content_fts", "nodes_fts", "photo_authored_fts"} {
				if table == index || strings.HasPrefix(table, index+"_") {
					exclusions[table] = "Derived search index"
				}
			}
			if exclusions[table] != "" {
				continue
			}
			var count int
			require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_temp_schema WHERE type='trigger' AND tbl_name=? AND name LIKE 'photo_events_%'`, table).Scan(&count))
			require.GreaterOrEqual(t, count, 3, "table %s needs capture or an exclusion reason", table)
		}
		return nil
	}))
}

func TestPhotoEventsQueryDefinitions(t *testing.T) {
	t.Parallel()
	for _, dependency := range []string{"saved", "collection"} {
		t.Run(dependency, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			run, err := s.BeginIngest(ctx, "cli", "Synthetic photos")
			require.NoError(t, err)
			_, _, err = s.IngestFile(ctx, run, s.RootID(), "photo.jpg", fakeHash("11"), 4, "image/jpeg", "/synthetic/photo.jpg", "")
			require.NoError(t, err)
			saved, err := s.CreateSavedQuery(ctx, "LiveSource", "", SavedQueryKindQuery, []byte(`{"text":"photo"}`))
			require.NoError(t, err)
			label, err := s.SetCollectionLabel(ctx, run.ID(), 1, new("Original"))
			require.NoError(t, err)
			text := "saved:LiveSource"
			if dependency == "collection" {
				text = "collection:Original"
			}
			q := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q}`, text))
			page, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: q}, nil)
			require.NoError(t, err)
			require.Len(t, page.Items, 1)
			events, cancel := s.SubscribePhotoEvents(ctx)
			defer cancel()
			if dependency == "saved" {
				_, err = s.UpdateSavedQuery(ctx, saved.ID, saved.Revision, SavedQueryPatch{Payload: new([]byte(`{"text":"absent"}`))})
			} else {
				_, err = s.SetCollectionLabel(ctx, run.ID(), label.Revision, new("Renamed"))
			}
			require.NoError(t, err)
			cancel()
			require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
			page, err = s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: q}, nil)
			if dependency == "saved" {
				require.NoError(t, err)
				require.Empty(t, page.Items)
			} else {
				require.ErrorContains(t, err, "unknown collection reference")
			}
		})
	}
}

func TestPhotoEventsHiddenAlbumMembership(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	hidden := browsePhotoNode(t, s, "hidden.jpg", fakeHash("11"), "image/jpeg")
	public := browsePhotoNode(t, s, "public.jpg", fakeHash("22"), "image/jpeg")
	privateAsset, err := s.PhotoAssetForNode(ctx, hidden.ID)
	require.NoError(t, err)
	privateAsset, err = s.SetPhotoAssetHidden(ctx, privateAsset.ID, privateAsset.Revision, true)
	require.NoError(t, err)
	publicAsset, err := s.PhotoAssetForNode(ctx, public.ID)
	require.NoError(t, err)
	album, err := s.CreatePhotoSet(ctx, "Trip")
	require.NoError(t, err)
	lockedEvents, stopLocked := s.SubscribePhotoEvents(ctx)
	defer stopLocked()
	openEvents, stopOpen := s.SubscribePhotoEvents(unlocked)
	defer stopOpen()
	stale := album
	for _, add := range []bool{true, false} {
		album, err = s.ChangePhotoSetMembers(unlocked, album.ID, album.Revision, add, PhotoSetSelection{AssetIDs: []string{privateAsset.ID}})
		require.NoError(t, err)
		event := nextPhotoEventWithAudience(t, lockedEvents)
		visible, err := s.VisiblePhotoEvent(ctx, event)
		require.NoError(t, err)
		require.Zero(t, visible)
		visible, err = s.VisiblePhotoEvent(unlocked, nextPhotoEventWithAudience(t, openEvents))
		require.NoError(t, err)
		require.Equal(t, PhotoEvent{AlbumID: album.ID, Revision: album.Revision}, visible)
		noPhotoEvent(t, lockedEvents)
		noPhotoEvent(t, openEvents)
		if add {
			album, err = s.UpdatePhotoSet(unlocked, album.ID, album.Revision, nil, nil, new(new(privateAsset.ID)))
			require.NoError(t, err)
			for _, events := range []<-chan PhotoEvent{lockedEvents, openEvents} {
				event, err := s.VisiblePhotoEvent(ctx, nextPhotoEventWithAudience(t, events))
				require.NoError(t, err)
				require.Equal(t, PhotoEvent{AlbumID: album.ID, Revision: album.Revision}, event)
			}
		}
	}
	_, err = s.UpdatePhotoSet(ctx, stale.ID, stale.Revision, new("Renamed"), nil, nil)
	require.ErrorIs(t, err, ErrStaleRevision)
	album, err = s.ChangePhotoSetMembers(ctx, album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: []string{publicAsset.ID}})
	require.NoError(t, err)
	for _, events := range []<-chan PhotoEvent{lockedEvents, openEvents} {
		event, err := s.VisiblePhotoEvent(ctx, nextPhotoEventWithAudience(t, events))
		require.NoError(t, err)
		require.Equal(t, PhotoEvent{AlbumID: album.ID, Revision: album.Revision}, event)
	}
	album, err = s.UpdatePhotoSet(ctx, album.ID, album.Revision, new("Renamed"), nil, nil)
	require.NoError(t, err)
	for _, events := range []<-chan PhotoEvent{lockedEvents, openEvents} {
		event, err := s.VisiblePhotoEvent(ctx, nextPhotoEventWithAudience(t, events))
		require.NoError(t, err)
		require.Equal(t, PhotoEvent{AlbumID: album.ID, Revision: album.Revision}, event)
	}
	for _, node := range []Node{hidden, public} {
		require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: node.BlobHash, Extractor: "synthetic", ExtractorVersion: 1, Status: ExtractionOK, Text: "updated"}))
		event, err := s.VisiblePhotoEvent(ctx, nextPhotoEventWithAudience(t, lockedEvents))
		require.NoError(t, err)
		if node.ID == hidden.ID {
			require.Zero(t, event)
		} else {
			require.Equal(t, PhotoEvent{Refetch: true}, event)
		}
		event, err = s.VisiblePhotoEvent(unlocked, nextPhotoEventWithAudience(t, openEvents))
		require.NoError(t, err)
		require.Equal(t, PhotoEvent{Refetch: true}, event)
	}
}
