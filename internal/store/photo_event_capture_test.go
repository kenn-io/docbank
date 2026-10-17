package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPhotoEventsCaptureAssetAlbumAndRefetchChanges(t *testing.T) {
	t.Parallel()
	for _, rule := range []string{"asset exclusion", "album membership", "tags", "tags/create", "tags/delete", "provenance", "metadata and indexing", "extraction", "directory refetch", "photo faces", "collection coverage", "visual previews", "global settings and Hidden credentials refetch"} {
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

			case "photo faces":
				_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, "11111111-1111-4111-8111-111111111111", document.FaceBox{X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4}, "")
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

func TestPhotoEventCaptureCoversPhotoSchemaTables(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	events, cancel := s.SubscribePhotoEvents(t.Context())
	defer cancel()
	_, err := s.CreateTag(t.Context(), "capture schema check")
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))

	triggers, err := s.writeDB.QueryContext(t.Context(), `SELECT name FROM temp.sqlite_schema WHERE type='trigger' AND name LIKE 'photo_events_%'`)
	require.NoError(t, err)
	defer func() { _ = triggers.Close() }()
	triggerNames := make(map[string]bool)
	for triggers.Next() {
		var name string
		require.NoError(t, triggers.Scan(&name))
		triggerNames[name] = true
	}
	require.NoError(t, triggers.Err())

	captured := make(map[string]bool)
	for _, rule := range photoEventCaptureRules() {
		for _, action := range []string{"INSERT", "UPDATE", "DELETE"} {
			trigger := fmt.Sprintf("photo_events_%s_%s", rule.table, action)
			require.True(t, triggerNames[trigger], "capture rule %q must install %s", rule.table, action)
		}
		if strings.HasPrefix(rule.table, "photo_") {
			captured[rule.table] = true
		}
	}

	quiet := map[string]string{
		"photo_change_receipts":          "operation receipts are read by the action that created them",
		"photo_authored_fts":             "FTS projection maintained by photo_files triggers",
		"photo_authored_fts_config":      "FTS projection maintained by photo_files triggers",
		"photo_authored_fts_content":     "FTS projection maintained by photo_files triggers",
		"photo_authored_fts_data":        "FTS projection maintained by photo_files triggers",
		"photo_authored_fts_docsize":     "FTS projection maintained by photo_files triggers",
		"photo_authored_fts_idx":         "FTS projection maintained by photo_files triggers",
		"photo_hidden_failures":          "authentication failure bookkeeping",
		"photo_hidden_lockout":           "authentication lockout bookkeeping",
		"photo_metadata_fts":             "FTS projection maintained by photo_technical_metadata triggers",
		"photo_metadata_fts_config":      "FTS projection maintained by photo_technical_metadata triggers",
		"photo_metadata_fts_content":     "FTS projection maintained by photo_technical_metadata triggers",
		"photo_metadata_fts_data":        "FTS projection maintained by photo_technical_metadata triggers",
		"photo_metadata_fts_docsize":     "FTS projection maintained by photo_technical_metadata triggers",
		"photo_metadata_fts_idx":         "FTS projection maintained by photo_technical_metadata triggers",
		"photo_sidecar_considered":       "import bookkeeping",
		"photo_technical_metadata_state": "projection recipe bookkeeping",
		"photo_timeline":                 "derived browse index maintained with its source writes",
		"photo_timeline_pending":         "derived browse index bookkeeping",
	}
	tables, err := s.db.QueryContext(t.Context(), `SELECT name FROM sqlite_schema WHERE type='table' AND name LIKE 'photo_%'`)
	require.NoError(t, err)
	defer func() { _ = tables.Close() }()
	schemaTables := make(map[string]bool)
	for tables.Next() {
		var name string
		require.NoError(t, tables.Scan(&name))
		schemaTables[name] = true
		if reason, ok := quiet[name]; ok {
			require.NotEmpty(t, reason)
			require.False(t, captured[name], "quiet table %q also has capture rules", name)
			continue
		}
		require.True(t, captured[name], "photo table %q needs capture rules or a documented quiet reason", name)
	}
	require.NoError(t, tables.Err())
	for table := range quiet {
		require.True(t, schemaTables[table], "quiet table %q must still exist in the schema", table)
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
	require.Zero(t, browsePhotoPage(t, s, `{"filters":{"has_duplicates":true}}`).Total)
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
	duplicates := browsePhotoPage(t, s, `{"filters":{"has_duplicates":true}}`)
	require.Equal(t, int64(1), duplicates.Total)
	require.Equal(t, first.ID, duplicates.Items[0].NodeID)
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
	require.Zero(t, browsePhotoPage(t, s, `{"filters":{"has_duplicates":true}}`).Total)
	doc, _, err = s.Restore(ctx, doc.ID, doc.Revision)
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
	doc, _, err = s.ReplaceContent(ctx, doc.ID, doc.Revision, second.BlobHash, 4, "application/octet-stream")
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
	duplicates = browsePhotoPage(t, s, `{"filters":{"has_duplicates":true}}`)
	require.Equal(t, int64(1), duplicates.Total)
	require.Equal(t, second.ID, duplicates.Items[0].NodeID)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id=?`, doc.ID)
		return err
	}))
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
	noPhotoEvent(t, events)
}

func TestPhotoEventsPhotoPeerMembership(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("11"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, first.ID)
	require.NoError(t, err)
	events, cancel := s.SubscribePhotoEvents(ctx)
	defer cancel()
	oldHash := first.BlobHash
	first, _, err = s.ReplaceContent(ctx, first.ID, first.Revision, fakeHash("55"), 4, "image/jpeg")
	require.NoError(t, err)
	require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
	_, err = s.CreateFile(ctx, s.RootID(), "old-photo-content.bin", oldHash, 4, "application/octet-stream")
	require.NoError(t, err)
	noPhotoEvent(t, events)
	_, err = s.CreateFile(ctx, s.RootID(), "new-photo-content.bin", first.BlobHash, 4, "application/octet-stream")
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
	first, _, err = s.Trash(ctx, first.ID, first.Revision)
	require.NoError(t, err)
	require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
	_, err = s.CreateFile(ctx, s.RootID(), "trashed-photo-content.bin", first.BlobHash, 4, "application/octet-stream")
	require.NoError(t, err)
	noPhotoEvent(t, events)
	first, _, err = s.Restore(ctx, first.ID, first.Revision)
	require.NoError(t, err)
	require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
	_, err = s.CreateFile(ctx, s.RootID(), "restored-photo-content.bin", first.BlobHash, 4, "application/octet-stream")
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
	asset, err = s.PhotoAssetForNode(ctx, first.ID)
	require.NoError(t, err)
	asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, asset.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
	_, err = s.CreateFile(ctx, s.RootID(), "detached-photo-content.bin", first.BlobHash, 4, "application/octet-stream")
	require.NoError(t, err)
	noPhotoEvent(t, events)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, first.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
	rejected := errors.New("synthetic rollback")
	require.ErrorIs(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE nodes SET trashed_at=? WHERE id=?`, nowRFC3339(), first.ID)
		if err != nil {
			return err
		}
		return rejected
	}), rejected)
	noPhotoEvent(t, events)
	_, err = s.CreateFile(ctx, s.RootID(), "reattached-photo-content.bin", first.BlobHash, 4, "application/octet-stream")
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
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
		require.Equal(t, PhotoEvent{Refetch: true}, visible)
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
		require.Equal(t, PhotoEvent{Refetch: true}, event)
		event, err = s.VisiblePhotoEvent(unlocked, nextPhotoEventWithAudience(t, openEvents))
		require.NoError(t, err)
		require.Equal(t, PhotoEvent{Refetch: true}, event)
	}
}
