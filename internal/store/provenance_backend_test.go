package store

import (
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func TestProvenancePhotoEffectsPostgres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	source := newTestStore(t)
	run, err := source.BeginCallerSuppliedIngest(ctx, "agent", "Synthetic photos")
	require.NoError(t, err)
	node, err := source.IngestFileExact(ctx, run, source.RootID(), "photo.jpg", fakeHash("d1"), 8, "image/jpeg", "/before/photo.jpg", "2024-01-02T03:04:05Z")
	require.NoError(t, err)
	bindings, err := source.ProvenanceVersionBindings(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	setup, owner := pgtest.NewDatabase(t)
	target := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	tables := append(append([]string{}, postgresEnrollmentCatalogs...), "blob_locations", "blob_packs", "blob_pack_entries", "photo_source_claims", "photo_timeline", "document_event_dirty")
	copyStoreCatalogFixture(t, source, setup, tables)
	target.primaryStoreID = source.primaryStoreID
	type projection struct {
		Path, Mtime, Day, Folder string
		Dirty, Queued            int
	}
	read := func(s *Store) projection {
		var p projection
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COALESCE(c.original_path,''),c.original_mtime,t.day,COALESCE(t.folder_key,''),
   (SELECT revision FROM document_event_dirty WHERE content_version_id=$1),
   (SELECT count(*) FROM photo_xmp_unsettled)
   FROM photo_files f JOIN photo_source_claims c ON c.file_id=f.file_id JOIN photo_timeline t ON t.file_id=f.file_id
   WHERE f.node_id=$2`, node.CurrentVersionID, node.ID).Scan(&p.Path, &p.Mtime, &p.Day, &p.Folder, &p.Dirty, &p.Queued))
		return p
	}
	before := read(source)
	require.Equal(t, before, read(target))
	var corrected [2]ProvenanceAppendResult
	for i, s := range []*Store{source, target} {
		corrected[i], err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{NodeID: node.ID, IfRevision: node.Revision,
			SourceKind: "agent", SourceDescription: "Corrected date", OriginalPath: "/corrected/photo.jpg", OriginalMTime: new("2024-02-03T10:00:00Z"), Supersedes: &bindings[0].ProvenanceIdentity})
		require.NoError(t, err)
		_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{NodeID: node.ID, IfRevision: corrected[i].Node.Revision, SourceKind: "agent", SourceDescription: "Duplicate correction", OriginalPath: "/wrong/photo.jpg", Supersedes: &bindings[0].ProvenanceIdentity})
		require.ErrorIs(t, err, ErrProvenanceMismatch)
		page, err := s.NodeProvenance(ctx, node.ID, 1, 1)
		require.NoError(t, err)
		require.Equal(t, 2, page.Total)
		require.Len(t, page.Items, 1)
		require.False(t, page.Items[0].Active)
	}
	reference := read(source)
	require.Empty(t, reference.Path, "caller-supplied evidence does not identify a filesystem folder")
	require.Equal(t, "2024-02-03", reference.Day)
	require.Equal(t, before.Dirty+1, reference.Dirty)
	require.Equal(t, reference, read(target))
	observation, err := source.BeginIngest(ctx, "cli", "New observation")
	require.NoError(t, err)
	for i, s := range []*Store{source, target} {
		receipt, changed, err := s.ReplaceContentForIngest(ctx, observation, node.ID, corrected[i].Node.Revision, node.BlobHash, node.Size, node.MimeType, "/observed/photo.jpg", "2024-03-04T10:00:00Z")
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, node.CurrentVersionID, receipt.Node.CurrentVersionID)
		require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
			high, err := s.auditNodeSequence(ctx, tx)
			if err != nil {
				return err
			}
			return s.validateMetadataState(ctx, tx, high)
		}))
	}
	reference = read(source)
	require.Equal(t, "/observed/photo.jpg", reference.Path)
	require.Equal(t, "2024-02-03", reference.Day, "the oldest retained observation still supplies the date")
	require.Equal(t, before.Dirty+2, reference.Dirty)
	require.Equal(t, reference, read(target))
}

func TestProvenanceInvalidationPostgres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	source := newTestStore(t)
	version := seedDocumentPeopleEvent(t, source, "note.txt", "e1", nil)
	input, err := source.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	_, err = source.PublishDocumentPeople(ctx, documentPeoplePublicationForInput(t, input))
	require.NoError(t, err)
	setup, owner := pgtest.NewDatabase(t)
	target := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	tables := append(append([]string{}, postgresEnrollmentCatalogs...), "blob_locations", "blob_packs", "blob_pack_entries",
		"document_event_state", "document_event_generations", "document_event_heads", "document_events", "document_event_actors", "document_event_primaries", "document_event_dirty")
	copyStoreCatalogFixture(t, source, setup, tables)
	target.primaryStoreID = source.primaryStoreID
	type projection struct{ EventHeads, Generations, Events, Actors, Primaries, PeopleHeads, People, EventEpoch, PeopleEpoch, Dirty, Facts int }
	read := func(s *Store) projection {
		var p projection
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT
   (SELECT count(*) FROM document_event_heads),(SELECT count(*) FROM document_event_generations),
   (SELECT count(*) FROM document_events),(SELECT count(*) FROM document_event_actors),(SELECT count(*) FROM document_event_primaries),
   (SELECT count(*) FROM document_people_heads),(SELECT count(*) FROM document_people),
   (SELECT publication_epoch FROM document_event_state),(SELECT publication_epoch FROM document_people_state),
   (SELECT revision FROM document_event_dirty WHERE content_version_id=$1),(SELECT count(*) FROM provenance)`, version.ID).
			Scan(&p.EventHeads, &p.Generations, &p.Events, &p.Actors, &p.Primaries, &p.PeopleHeads, &p.People, &p.EventEpoch, &p.PeopleEpoch, &p.Dirty, &p.Facts))
		return p
	}
	before := read(source)
	require.Equal(t, 1, before.EventHeads)
	require.Equal(t, 1, before.PeopleHeads)
	require.Positive(t, before.People)
	require.Equal(t, before, read(target))
	observation, err := source.BeginIngest(ctx, "test", "Repeated observation")
	require.NoError(t, err)
	observe := func(s *Store) error {
		_, added, err := s.IngestFileWithMembership(ctx, observation, s.RootID(), "note.txt", version.BlobHash, version.Size, version.MimeType, "/other/note.txt", "")
		if err == nil {
			require.False(t, added)
		}
		return err
	}
	// Fail after heads and people have been removed, before dirty-state publication.
	var role string
	require.NoError(t, target.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role))
	quotedRole := pgx.Identifier{role}.Sanitize()
	_, err = setup.ExecContext(ctx, "REVOKE INSERT ON documents.document_event_dirty FROM "+quotedRole)
	require.NoError(t, err)
	require.ErrorContains(t, observe(target), "permission denied")
	require.Equal(t, before, read(target), "failure must roll back evidence, projections and their epochs together")
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON documents.document_event_dirty TO "+quotedRole)
	require.NoError(t, err)
	require.NoError(t, observe(source))
	require.NoError(t, observe(target))
	after := read(source)
	require.Equal(t, 0, after.EventHeads)
	require.Equal(t, 0, after.Generations)
	require.Equal(t, 0, after.Events)
	require.Equal(t, 0, after.Actors)
	require.Equal(t, 0, after.Primaries)
	require.Equal(t, 0, after.PeopleHeads)
	require.Equal(t, 0, after.People)
	require.Equal(t, before.EventEpoch+1, after.EventEpoch)
	require.Equal(t, before.PeopleEpoch+1, after.PeopleEpoch)
	require.Equal(t, before.Dirty+1, after.Dirty)
	require.Equal(t, before.Facts+1, after.Facts)
	require.Equal(t, after, read(target))
}

func TestImportReadSnapshotsPostgres(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"provenance", "collection"} {
		t.Run(kind, func(t *testing.T) {
			s := newPostgresQueryStore(t)
			ctx := t.Context()
			run, err := s.BeginIngest(ctx, "cli", "Snapshot")
			require.NoError(t, err)
			node, err := s.IngestFileExact(ctx, run, s.RootID(), "note.txt", fakeHash("f1"), 4, "text/plain", "/source/note.txt", "")
			require.NoError(t, err)
			read := func(s *Store) (any, error) {
				if kind == "provenance" {
					return s.NodeProvenance(ctx, node.ID, 10, 0)
				}
				return s.CollectionMembers(ctx, run.ID(), 10, 0)
			}
			before, err := read(s)
			require.NoError(t, err)
			conn, err := s.db.Conn(ctx)
			require.NoError(t, err)
			var config *pgx.ConnConfig
			err = conn.Raw(func(driverConn any) error {
				pgConn, ok := driverConn.(*stdlib.Conn)
				require.True(t, ok)
				config = pgConn.Conn().Config().Copy()
				return nil
			})
			require.NoError(t, conn.Close())
			require.NoError(t, err)
			config.Tracer = &afterFirstSelect{run: func() {
				_, _, err := s.Move(ctx, node.ID, s.RootID(), "renamed.txt", node.Revision)
				require.NoError(t, err)
				_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{NodeID: node.ID, IfRevision: UnconditionalRev, SourceKind: "agent", SourceDescription: "New fact", OriginalPath: "new-source"})
				require.NoError(t, err)
				_, err = s.IngestFileExact(ctx, run, s.RootID(), "second.txt", fakeHash("f2"), 8, "text/plain", "/source/second.txt", "")
				require.NoError(t, err)
			}}
			db := stdlib.OpenDB(*config)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			snapshot := &Store{db: db, rootID: s.rootID, postgresSchema: s.postgresSchema}
			during, err := read(snapshot)
			require.NoError(t, err)
			require.Equal(t, before, during)
			after, err := read(s)
			require.NoError(t, err)
			require.NotEqual(t, before, after)
		})
	}
}

func TestProvenanceRequeuesPhotoExportPostgres(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"append", "observe"} {
		t.Run(operation, func(t *testing.T) {
			source, asset, raw := xmpSidecarPhoto(t)
			importXMP(t, source, raw.ID, PhotoAuthored{Rating: 3, Confirmed: PhotoConfirmedRating})
			require.Empty(t, xmpTargets(t, source))
			setup, owner := pgtest.NewDatabase(t)
			target := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
			tables := append(append([]string{}, postgresEnrollmentCatalogs...), "blob_locations", "blob_packs", "blob_pack_entries", "photo_source_claims", "photo_timeline")
			copyStoreCatalogFixture(t, source, setup, tables)
			target.primaryStoreID = source.primaryStoreID
			ctx := t.Context()
			for _, s := range []*Store{source, target} {
				node, err := s.NodeByID(ctx, raw.NodeID)
				require.NoError(t, err)
				if operation == "append" {
					_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent", SourceDescription: "Photo evidence", OriginalPath: "opaque-reference"})
				} else {
					var run IngestRun
					run, err = s.BeginIngest(ctx, "cli", "Photo observation")
					require.NoError(t, err)
					_, _, err = s.ReplaceContentForIngest(ctx, run, node.ID, node.Revision, node.BlobHash, node.Size, node.MimeType, "/source/photo.cr2", "")
				}
				require.NoError(t, err)
				var queued int
				require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM photo_xmp_unsettled WHERE asset_id=$1`, asset.ID).Scan(&queued))
				require.Equal(t, 1, queued, "changing provenance must requeue a previously settled asset")
			}
		})
	}
}
