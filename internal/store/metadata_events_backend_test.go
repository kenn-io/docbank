package store

import (
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkSourceMetadataPhotoEffectsPostgres(t *testing.T) {
	t.Helper()
	// The retained XMP receipt is produced by SQLite. Both engines then execute
	// native metadata publication/reprojection against that settled receipt.
	source, asset, raw := xmpSidecarPhoto(t)
	importXMP(t, source, raw.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating})
	require.Empty(t, xmpTargets(t, source))
	target, _ := copyRetentionStore(t, source)
	ctx := t.Context()
	node, err := source.NodeByID(ctx, raw.NodeID)
	require.NoError(t, err)
	canonical := photoCanonical(t,
		photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp("2024-05-06T12:34:56Z", "2024-05-06T12:34:56Z", document.SourceMetadataPrecisionSecond, document.SourceMetadataTimezoneUTC, "")),
		photoMetadataField("image.exif.orientation", "image.exif", "Orientation", photoInteger(2)),
	)
	read := func(t *testing.T, s *Store) []string {
		t.Helper()
		var result []string
		require.NoError(t, queryRows(ctx, s.db, `SELECT file_id,sort_key,day,COALESCE(modified_time,''),COALESCE(folder_key,'') FROM photo_timeline ORDER BY file_id`, nil, func(rows *sql.Rows) error {
			var file, sortKey, day, modified, folder string
			if err := rows.Scan(&file, &sortKey, &day, &modified, &folder); err != nil {
				return err
			}
			result = append(result, file, sortKey, day, modified, folder)
			return nil
		}))
		require.NoError(t, queryRows(ctx, s.db, `SELECT file_id,token FROM photo_xmp_inputs ORDER BY file_id`, nil, func(rows *sql.Rows) error {
			var file, token string
			if err := rows.Scan(&file, &token); err != nil {
				return err
			}
			result = append(result, file, token)
			return nil
		}))
		require.NoError(t, queryRows(ctx, s.db, `SELECT asset_id FROM photo_xmp_unsettled ORDER BY asset_id`, nil, func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			result = append(result, id)
			return nil
		}))
		return result
	}
	before := read(t, source)
	require.Equal(t, before, read(t, target))
	for _, s := range []*Store{source, target} {
		_, err := s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("a8"), canonical)
		require.NoError(t, err)
	}
	after := read(t, source)
	require.NotEqual(t, before, after)
	require.Equal(t, after, read(t, target))
	var day string
	require.NoError(t, target.db.QueryRowContext(ctx, `SELECT day FROM photo_timeline WHERE file_id=$1`, raw.ID).Scan(&day))
	require.Equal(t, "2024-05-06", day)
	require.Contains(t, after, asset.ID, "changed extracted evidence requeues the settled asset")
	for _, recovery := range []string{"republication", "reprojection"} {
		t.Run(recovery, func(t *testing.T) {
			// An older projection recipe can omit these facts. Copy that retained
			// state, including SQLite's resulting timeline and receipt token.
			_, err := source.db.ExecContext(ctx, `DELETE FROM photo_technical_metadata
				WHERE generation_id=(SELECT generation_id FROM source_metadata_heads WHERE source_sha256=$1)`, node.BlobHash)
			require.NoError(t, err)
			stale := read(t, source)
			require.NotEqual(t, after, stale)
			_, err = source.db.ExecContext(ctx, `UPDATE photo_technical_metadata_state SET projection_recipe='photo-technical/v0'`)
			require.NoError(t, err)
			recovering, _ := copyRetentionStore(t, source)
			require.Equal(t, stale, read(t, recovering))
			for _, s := range []*Store{source, recovering} {
				if recovery == "republication" {
					_, err := s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("a8"), canonical)
					require.NoError(t, err)
				} else {
					require.NoError(t, s.refreshPhotoTechnicalMetadata(ctx))
				}
				require.Equal(t, after, read(t, s), "recovery must refresh timeline and export work")
			}
		})
	}
	for _, s := range []*Store{source, target} {
		// Returning to the original generation also restores the receipt token.
		publishXMPOrientation(t, s, raw.NodeID, fakeHash("ee"))
	}
	require.Equal(t, before, read(t, source))
	require.Equal(t, before, read(t, target))
	require.Empty(t, xmpTargets(t, target))
}

func checkMetadataEventSnapshotsPostgres(t *testing.T) {
	t.Helper()
	for _, operation := range []string{"node by ID", "node by path", "version metadata", "photo metadata", "event evidence", "coverage", "rebuild coverage", "work targets"} {
		t.Run(operation, func(t *testing.T) {
			s := newPostgresQueryStore(t)
			ctx := t.Context()
			node, err := s.CreateFile(ctx, s.RootID(), "snapshot.jpg", fakeHash("a9"), 1, "image/jpeg")
			require.NoError(t, err)
			canonical := photoCanonical(t, photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Snapshot")))
			_, err = s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("b9"), canonical)
			require.NoError(t, err)
			target := requireDocumentEventTarget(t, s, node.CurrentVersionID)
			record := documentEventRecord(t, s.VaultID(), node.CurrentVersionID, "d9")
			record.DocumentKind = "image"
			_, err = s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
			require.NoError(t, err)
			read := func(s *Store) (any, error) {
				switch operation {
				case "node by ID":
					return s.NodeSourceMetadataViewByID(ctx, node.ID)
				case "node by path":
					return s.NodeSourceMetadataViewByPath(ctx, "/snapshot.jpg")
				case "version metadata":
					return s.ContentVersionSourceMetadata(ctx, node.CurrentVersionID)
				case "photo metadata":
					return s.ContentVersionPhotoMetadata(ctx, node.CurrentVersionID)
				case "event evidence":
					return s.LoadDocumentEventEvidence(ctx, target)
				case "coverage":
					return s.DocumentEventCoverageReport(ctx)
				case "rebuild coverage":
					return s.DocumentEventRebuildCoverage(ctx)
				default:
					return s.MissingDocumentEventTargetsAfter(ctx, DocumentEventsDeriverFingerprint, "", 10)
				}
			}
			before, err := read(s)
			require.NoError(t, err)
			conn, err := s.db.Conn(ctx)
			require.NoError(t, err)
			var cfg *pgx.ConnConfig
			err = conn.Raw(func(c any) error {
				pgConn, ok := c.(*stdlib.Conn)
				require.True(t, ok)
				cfg = pgConn.Conn().Config().Copy()
				return nil
			})
			require.NoError(t, conn.Close())
			require.NoError(t, err)
			cfg.Tracer = &afterFirstSelect{run: func() {
				_, err := s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("c9"), canonical)
				require.NoError(t, err)
			}}
			db := stdlib.OpenDB(*cfg)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			reader := &Store{db: db, rootID: s.rootID, vaultID: s.vaultID, postgresSchema: s.postgresSchema}
			during, err := read(reader)
			require.NoError(t, err)
			require.Equal(t, before, during)
			after, err := read(s)
			require.NoError(t, err)
			require.NotEqual(t, before, after)
		})
	}
}

func checkMetadataPublicationRollbackPostgres(t *testing.T) {
	t.Helper()
	for _, blockedTable := range []string{"photo_technical_metadata", "document_event_actors", "document_event_builds"} {
		t.Run(blockedTable, func(t *testing.T) {
			setup, owner := pgtest.NewDatabase(t)
			s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
			ctx := t.Context()
			node, err := s.CreateFile(ctx, s.RootID(), "rollback.txt", fakeHash("a7"), 1, "text/plain")
			require.NoError(t, err)
			require.NoError(t, s.EnsureDocumentEventRecipe(ctx, DocumentEventsDeriverFingerprint))
			before, err := s.DocumentEventStateRow(ctx, DocumentEventsDeriverFingerprint)
			require.NoError(t, err)
			var role string
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
			_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents."+blockedTable+" FROM "+pgx.Identifier{role}.Sanitize())
			require.NoError(t, err)
			publish := func() error {
				switch blockedTable {
				case "photo_technical_metadata":
					_, err := s.PublishSourceMetadata(ctx, node.BlobHash, fakeHash("b7"), photoCanonical(t, photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Rollback"))))
					return err
				case "document_event_actors":
					target := requireDocumentEventTarget(t, s, node.CurrentVersionID)
					_, err := s.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, documentEventRecord(t, s.VaultID(), node.CurrentVersionID, "c7")))
					return err
				default:
					_, err := s.StartDocumentEventRebuild(ctx, "77777777-7777-4777-8777-777777777777", fakeHash("d7"))
					return err
				}
			}
			require.ErrorContains(t, publish(), "permission denied")
			for _, table := range []string{"source_metadata_generations", "source_metadata_heads", "document_event_generations", "document_event_heads", "document_events", "document_event_actors", "document_event_attempts", "document_event_builds"} {
				var count int
				require.NoError(t, s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
				require.Zero(t, count, table)
			}
			after, err := s.DocumentEventStateRow(ctx, DocumentEventsDeriverFingerprint)
			require.NoError(t, err)
			require.Equal(t, before, after)
			_, err = setup.ExecContext(ctx, "GRANT INSERT ON documents."+blockedTable+" TO "+pgx.Identifier{role}.Sanitize())
			require.NoError(t, err)
			require.NoError(t, publish(), "the same operation succeeds after the missing grant is restored")
		})
	}
}
