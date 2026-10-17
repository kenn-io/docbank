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
