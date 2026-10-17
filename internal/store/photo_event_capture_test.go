package store

import (
	"database/sql"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"testing"
)

func TestPhotoEventsCaptureWrites(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"photo file", "technical metadata", "published metadata", "quality worker", "album", "tag", "saved", "collection", "document", "document worker", "peer leaves", "matching hash", "unrelated hash"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			run, err := s.BeginIngest(ctx, "cli", "Synthetic")
			require.NoError(t, err)
			photo, _, err := s.IngestFile(ctx, run, s.RootID(), "photo.jpg", fakeHash("11"), 4, "image/jpeg", "/synthetic/photo.jpg", "")
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, photo.ID)
			require.NoError(t, err)
			docHash := fakeHash("22")
			if change == "peer leaves" {
				docHash = photo.BlobHash
			}
			doc, err := s.CreateFile(ctx, s.RootID(), "document.txt", docHash, 4, "text/plain")
			require.NoError(t, err)
			tag, err := s.CreateTag(ctx, "Keyword")
			require.NoError(t, err)
			metadata := document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{{Key: "image.exif.iso", Namespace: "image.exif", SourceField: "ISO", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataInteger, Integer: new(int64(200))}}}}
			canonical, _, err := document.MarshalSourceMetadataV1(metadata)
			require.NoError(t, err)
			generation, err := s.PublishSourceMetadata(ctx, photo.BlobHash, fakeHash("f1"), canonical)
			require.NoError(t, err)
			ch, stop := s.SubscribePhotoEvents()
			defer stop()
			generic := false
			quiet := false
			switch change {
			case "photo file":
				err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `UPDATE photo_files SET file_id='replacement-photo',revision=revision+1 WHERE node_id=?`, photo.ID)
					if err != nil {
						return err
					}
					_, err = tx.ExecContext(ctx, `UPDATE photo_assets SET display_file_id='replacement-photo',revision=revision+1 WHERE asset_id=?`, asset.ID)
					return err
				})
			case "technical metadata":
				err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `UPDATE photo_technical_metadata SET iso=400 WHERE generation_id=?`, generation.GenerationID)
					return err
				})
			case "published metadata":
				_, err = s.PublishSourceMetadata(ctx, photo.BlobHash, fakeHash("f2"), photoCanonical(t,
					photoMetadataField("image.exif.iso", "image.exif", "ISO", photoInteger(400))))
			case "quality worker":
				fingerprints, e := document.CurrentPhotoQualityFingerprints()
				require.NoError(t, e)
				err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `INSERT INTO photo_quality_signals(content_version_id,evaluator_fingerprint,state) VALUES(?,?,'unavailable')`, photo.CurrentVersionID, fingerprints.Evaluator)
					return err
				})
			case "album":
				_, err = s.CreatePhotoSet(ctx, "Album")
				generic = true
			case "tag":
				_, err = s.RenameTag(ctx, tag.ID, tag.Revision, "Renamed")
				generic = true
			case "saved":
				_, err = s.CreateSavedQuery(ctx, "PhotoSource", "", SavedQueryKindQuery, []byte(`{"text":"photo"}`))
				generic = true
			case "collection":
				_, err = s.SetCollectionLabel(ctx, run.ID(), 1, new("Renamed"))
				generic = true
			case "document":
				_, _, err = s.Move(ctx, doc.ID, s.RootID(), "renamed.txt", doc.Revision)
				quiet = true
			case "document worker":
				err = s.RecordExtraction(ctx, ExtractionResult{BlobHash: doc.BlobHash, Extractor: "synthetic", ExtractorVersion: 1, Status: ExtractionOK, Text: "Document"})
				quiet = true
			case "peer leaves":
				_, _, err = s.Trash(ctx, doc.ID, doc.Revision)
				generic = true
			case "matching hash", "unrelated hash":
				hash := fakeHash("33")
				if change == "matching hash" {
					hash = photo.BlobHash
					generic = true
				} else {
					quiet = true
				}
				_, err = s.CreateFile(ctx, s.RootID(), "peer.txt", hash, 4, "text/plain")
			}
			require.NoError(t, err)
			if quiet {
				noPhotoEvent(t, ch)
				return
			}
			event := nextPhotoEvent(t, ch)
			if generic {
				require.Equal(t, PhotoEvent{Refetch: true}, event)
			} else {
				require.Equal(t, asset.ID, event.AssetID)
				current, e := s.PhotoAssetByID(ctx, asset.ID)
				require.NoError(t, e)
				require.Equal(t, current.Revision, event.Revision)
			}
			noPhotoEvent(t, ch)
		})
	}
}

func TestPhotoEventsSharedSourceOverflow(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"source", "generation"} {
		t.Run(kind, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			run, err := s.BeginIngest(ctx, "cli", "Synthetic")
			require.NoError(t, err)
			for i := range 70 {
				_, _, err := s.IngestFile(ctx, run, s.RootID(), fmt.Sprintf("photo-%d.jpg", i), fakeHash("11"), 4, "image/jpeg", "/synthetic/photo.jpg", "")
				require.NoError(t, err)
			}
			metadata := document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{{Key: "image.exif.iso", Namespace: "image.exif", SourceField: "ISO", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataInteger, Integer: new(int64(200))}}}}
			canonical, _, err := document.MarshalSourceMetadataV1(metadata)
			require.NoError(t, err)
			generation, err := s.PublishSourceMetadata(ctx, fakeHash("11"), fakeHash("f1"), canonical)
			require.NoError(t, err)
			ch, stop := s.SubscribePhotoEvents()
			defer stop()
			require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				if kind == "generation" {
					_, err := tx.ExecContext(ctx, `UPDATE photo_technical_metadata SET iso=400 WHERE generation_id=?`, generation.GenerationID)
					return err
				}
				_, err := tx.ExecContext(ctx, `DELETE FROM source_metadata_heads WHERE source_sha256=?`, fakeHash("11"))
				return err
			}))
			require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, ch))
			noPhotoEvent(t, ch)
		})
	}
}

func TestPhotoEventsDocumentSeedingQuiet(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	for i := range 70 {
		_, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("document-%d.txt", i), fakeHash("22"), 4, "text/plain")
		require.NoError(t, err)
	}
	photo, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("11"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, photo.ID)
	require.NoError(t, err)
	events, stop := s.SubscribePhotoEvents()
	defer stop()
	require.NoError(t, s.SeedTextExtractionQueue(ctx, legacyPlainTextExtractor, legacyPlainTextExtractorVersion))
	noPhotoEvent(t, events)
	_, _, err = s.Move(ctx, photo.ID, s.RootID(), "renamed.jpg", photo.Revision)
	require.NoError(t, err)
	require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
	noPhotoEvent(t, events)
}

func TestPhotoEventsDocumentMetadataBudget(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"source", "generation"} {
		t.Run(kind, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			photo, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("11"), 4, "image/jpeg")
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, photo.ID)
			require.NoError(t, err)
			hashes := []string{photo.BlobHash}
			for i := range 70 {
				hash := browseHash(fmt.Sprintf("document-metadata-%d", i))
				_, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("document-%d.txt", i), hash, 4, "text/plain")
				require.NoError(t, err)
				hashes = append(hashes, hash)
			}
			canonical := photoCanonical(t, photoMetadataField("image.exif.iso", "image.exif", "ISO", photoInteger(200)))
			for _, hash := range hashes {
				_, err := s.PublishSourceMetadata(ctx, hash, fakeHash("f1"), canonical)
				require.NoError(t, err)
			}
			events, stop := s.SubscribePhotoEvents()
			defer stop()
			require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				mutation := `DELETE FROM source_metadata_heads`
				if kind == "generation" {
					mutation = `UPDATE photo_technical_metadata SET iso=400`
				}
				_, err := tx.ExecContext(ctx, mutation)
				return err
			}))
			require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
			noPhotoEvent(t, events)
		})
	}
}

// Exercise single-source writes as well as the public multi-table operations above:
// another captured table in the same transaction must not hide a missing source.
func TestPhotoEventsViewSources(t *testing.T) {
	t.Parallel()
	for _, source := range []struct {
		table, mutation string
		refetch         bool
	}{
		{"photo_assets", `UPDATE photo_assets SET revision=revision+1`, false},
		{"photo_files", `UPDATE photo_files SET rating=3,revision=revision+1`, false},
		{"nodes", `UPDATE nodes SET name='renamed.jpg',revision=revision+1 WHERE name='photo.jpg'`, false},
		{"node_tags", `DELETE FROM node_tags`, false},
		{"provenance", `DELETE FROM provenance`, false},
		{"extracted_text", `UPDATE extracted_text SET text='Changed text'`, false},
		{"text_searchable_versions", `INSERT INTO text_searchable_versions SELECT current_version_id FROM nodes WHERE name='photo.jpg'`, false},
		{"photo_faces", `INSERT INTO photo_faces(face_id,content_version_id,x,y,width,height,created_at) SELECT '11111111-1111-4111-8111-111111111111',current_version_id,0,0,0.5,0.5,'2026-01-01T00:00:00Z' FROM nodes WHERE name='photo.jpg'`, false},
		{"visual_preview_generations", `DELETE FROM visual_preview_generations`, false},
		{"visual_preview_heads", `DELETE FROM visual_preview_heads`, false},
		{"person_document_assertions", `UPDATE person_document_assertions SET note='Changed note',revision=revision+1`, false},
		{"photo_set_members", `INSERT INTO photo_set_members SELECT set_id,asset_id,'2026-01-01T00:00:00Z' FROM photo_sets CROSS JOIN photo_assets`, true},
		{"person_aliases", `INSERT INTO person_aliases SELECT person_id,NULL,'retired','2026-01-01T00:00:00Z' FROM persons`, true},
		{"persons", `UPDATE persons SET display_name='Changed',display_name_folded='changed',revision=revision+1`, true},
		{"photo_library_settings", `INSERT INTO photo_library_settings VALUES(1,NULL,1,'2026-01-01T00:00:00Z')`, true},
		{"photo_hidden_credentials", `INSERT INTO photo_hidden_credentials VALUES(1,'synthetic-passcode-hash')`, true},
	} {
		t.Run(source.table, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			run, err := s.BeginIngest(ctx, "cli", "Synthetic")
			require.NoError(t, err)
			photo, _, err := s.IngestFile(ctx, run, s.RootID(), "photo.jpg", fakeHash("11"), 4, "image/jpeg", "/synthetic/photo.jpg", "")
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, photo.ID)
			require.NoError(t, err)
			tag, err := s.CreateTag(ctx, "Keyword")
			require.NoError(t, err)
			_, err = s.AssignTag(ctx, tag.ID, photo.ID, photo.Revision)
			require.NoError(t, err)
			person, err := s.CreatePerson(ctx, "Synthetic", "operator")
			require.NoError(t, err)
			_, err = s.MarkPersonInPhoto(ctx, photo.CurrentVersionID, person.PersonID)
			require.NoError(t, err)
			_, err = s.CreatePhotoSet(ctx, "Album")
			require.NoError(t, err)
			require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: photo.BlobHash, Extractor: "synthetic", ExtractorVersion: 1, Status: ExtractionOK, Text: "Original text"}))
			_, err = s.PublishVisualPreview(ctx, photo.CurrentVersionID, readyVisualPreview(t, photo.BlobHash, fakeHash("22"), 9), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
			require.NoError(t, err)
			if source.table == "visual_preview_generations" {
				_, err = s.db.ExecContext(ctx, `DELETE FROM visual_preview_heads`)
				require.NoError(t, err)
			}
			events, stop := s.SubscribePhotoEvents()
			defer stop()
			require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				result, err := tx.ExecContext(ctx, source.mutation)
				if err != nil {
					return err
				}
				rows, err := result.RowsAffected()
				require.NoError(t, err)
				require.EqualValues(t, 1, rows, "the source write must reach a row")
				return nil
			}))
			if source.refetch {
				require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
			} else {
				require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
			}
			noPhotoEvent(t, events)
		})
	}
}

func TestPhotoEventsRenditionSources(t *testing.T) {
	t.Parallel()
	for _, source := range []struct{ table, mutation string }{
		{"rendition_heads", `DELETE FROM rendition_heads`},
		{"rendition_jobs", `UPDATE rendition_jobs SET updated_at='2026-08-22T12:00:00.000000000Z'`},
		{"rendition_job_waiters", `UPDATE rendition_job_waiters SET state='failed'`},
	} {
		t.Run(source.table, func(t *testing.T) {
			s, _ := newRenditionCatalogFixture(t)
			ctx := t.Context()
			photo, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", catalogSourceHash, 20, "image/jpeg")
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, photo.ID)
			require.NoError(t, err)
			profile := catalogProcessingProfile(t, false)
			build := catalogRenditionBuild(s, profile)
			require.NoError(t, s.StageRenditionBuild(ctx, build))
			require.NoError(t, publishAttachmentForTest(t, s, RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: photo.CurrentVersionID, BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z"}))
			request := renditionJobTestRequest(photo.CurrentVersionID, profile)
			grantRenditionJobConsent(t, s, request)
			_, _, err = s.EnqueueRenditionJob(ctx, request)
			require.NoError(t, err)
			events, stop := s.SubscribePhotoEvents()
			defer stop()
			require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				result, err := tx.ExecContext(ctx, source.mutation)
				if err != nil {
					return err
				}
				rows, err := result.RowsAffected()
				require.NoError(t, err)
				require.EqualValues(t, 1, rows)
				return nil
			}))
			require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
			noPhotoEvent(t, events)
		})
	}
}

// Every photo table must either invalidate a viewer or have an explicit reason
// to stay quiet. Read installed triggers so a missing rule fails at the schema boundary.
func TestPhotoEventsSchemaCoverage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, stop := s.SubscribePhotoEvents()
	defer stop()
	quiet := map[string]string{
		"photo_metadata_fts":             "derived technical metadata search index",
		"photo_authored_fts":             "derived authored metadata search index",
		"photo_technical_metadata_state": "worker attempts do not change published metadata",
		"photo_timeline":                 "derived browse index",
		"photo_timeline_pending":         "index work queue",
		"photo_sidecar_considered":       "sidecar discovery bookkeeping",
		"photo_change_receipts":          "write history rather than current photo state",
		"photo_hidden_lockout":           "access polling owns lockout state",
		"photo_hidden_failures":          "failed passcode attempts do not change photo state",
	}
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT name, EXISTS(SELECT 1 FROM temp.sqlite_schema capture WHERE capture.type='trigger' AND capture.tbl_name=main.name AND capture.name LIKE 'photo_events_%') FROM pragma_table_list main WHERE schema='main' AND type IN ('table','virtual') AND substr(name,1,6)='photo_'`)
		if err != nil {
			return err
		}
		defer func() { require.NoError(t, rows.Close()) }()
		for rows.Next() {
			var table string
			var captured bool
			require.NoError(t, rows.Scan(&table, &captured))
			if reason, excluded := quiet[table]; excluded {
				require.False(t, captured, "%s stays quiet: %s", table, reason)
				delete(quiet, table)
			} else {
				require.True(t, captured, "%s needs photo change capture or an explicit quiet classification", table)
			}
		}
		return rows.Err()
	}))
	require.Empty(t, quiet, "quiet classifications must refer to current schema tables")
}
