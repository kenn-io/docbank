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
	for _, rule := range []string{"asset exclusion", "album membership", "tags", "provenance", "metadata and indexing", "extraction", "directory refetch", "collection coverage", "visual previews", "global settings and Hidden credentials refetch"} {
		t.Run(rule, func(t *testing.T) {
			ctx := t.Context()
			if rule == "collection coverage" {
				s, run, docs, profile := collectionCoverageFixture(t, 1)
				node, _, err := s.IngestFile(ctx, run, s.RootID(), "photo.jpg", fakeHash("11"), 4, "image/jpeg", "/synthetic/photo.jpg", "")
				require.NoError(t, err)
				asset, err := s.PhotoAssetForNode(ctx, node.ID)
				require.NoError(t, err)
				events, cancel := s.SubscribePhotoEvents(ctx)
				t.Cleanup(cancel)
				collectionCoveragePublish(t, s, docs[0], profile, "complete")
				noPhotoEvent(t, events)
				collectionCoveragePublish(t, s, node, profile, "complete")
				require.Equal(t, PhotoEvent{AssetID: asset.ID}, nextPhotoEvent(t, events))
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

			case "tags":
				_, err = s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
				require.NoError(t, err)
				require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
				noPhotoEvent(t, events)
				_, err = s.RenameTag(ctx, tag.ID, tag.Revision+1, "holiday")
				require.NoError(t, err)
				require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
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
				require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)

			case "metadata and indexing":
				metadata := document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{{Key: "image.exif.iso", Namespace: "image.exif", SourceField: "ISO", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataInteger, Integer: new(int64(200))}}}}
				canonical, _, err := document.MarshalSourceMetadataV1(metadata)
				require.NoError(t, err)
				generation, err := s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("f1"), canonical)
				require.NoError(t, err)
				require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
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
					require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID, statement)
					noPhotoEvent(t, events)
				}

			case "extraction":
				require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: node.BlobHash, Extractor: "synthetic", ExtractorVersion: 1, Status: ExtractionOK, Text: "Trip"}))
				require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)

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
				require.Equal(t, PhotoEvent{AssetID: asset.ID}, nextPhotoEvent(t, events))
				_, err = s.PublishVisualPreview(ctx, node.CurrentVersionID, canonical, physical)
				require.NoError(t, err)
				noPhotoEvent(t, events)
				recipe := visualPreviewRecipe()
				recipe.ProcessorFingerprint = fakeHash("33")
				another := readyVisualPreviewWithRecipe(t, node.BlobHash, fakeHash("44"), 9, recipe)
				_, err = s.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, another, physical)
				require.NoError(t, err)
				require.Equal(t, PhotoEvent{AssetID: asset.ID}, nextPhotoEvent(t, events))
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
	a, err := s.PhotoAssetForNode(ctx, first.ID)
	require.NoError(t, err)
	b, err := s.PhotoAssetForNode(ctx, second.ID)
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
	require.Equal(t, a.ID, nextPhotoEvent(t, events).AssetID)
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
	require.Equal(t, a.ID, nextPhotoEvent(t, events).AssetID)
	doc, _, err = s.Restore(ctx, doc.ID, doc.Revision)
	require.NoError(t, err)
	require.Equal(t, a.ID, nextPhotoEvent(t, events).AssetID)
	doc, _, err = s.ReplaceContent(ctx, doc.ID, doc.Revision, second.BlobHash, 4, "application/octet-stream")
	require.NoError(t, err)
	ids := []string{nextPhotoEvent(t, events).AssetID, nextPhotoEvent(t, events).AssetID}
	require.ElementsMatch(t, []string{a.ID, b.ID}, ids)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id=?`, doc.ID)
		return err
	}))
	require.Equal(t, b.ID, nextPhotoEvent(t, events).AssetID)
	noPhotoEvent(t, events)
}

func TestPhotoEventsCaptureDepictedPeople(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "portrait.jpg", fakeHash("11"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	person, err := s.CreatePerson(ctx, "Person A", "operator")
	require.NoError(t, err)
	events, cancel := s.SubscribePhotoEvents(ctx)
	t.Cleanup(cancel)
	assertion, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{AssetID: asset.ID}, nextPhotoEvent(t, events))
	noPhotoEvent(t, events)
	for _, statement := range []string{
		`UPDATE person_document_assertions SET action='withdrawn',revision=revision+1 WHERE assertion_id=?`,
		`DELETE FROM person_document_assertions WHERE assertion_id=?`,
	} {
		require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, statement, assertion.AssertionID)
			return err
		}))
		require.Equal(t, PhotoEvent{AssetID: asset.ID}, nextPhotoEvent(t, events))
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
