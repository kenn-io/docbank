package store

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func watchPhotoChanges(t *testing.T, s *Store) <-chan []PhotoEvent {
	t.Helper()
	events, stop := s.SubscribePhotoEvents()
	t.Cleanup(stop)
	return events
}
func nextPhotoWake(t *testing.T, ch <-chan []PhotoEvent) []PhotoEvent {
	t.Helper()
	select {
	case events := <-ch:
		return events
	default:
		t.Fatal("missing committed photo wake")
	}
	return nil
}
func TestPhotoEventsCaptureAcrossConnectionsAndUnreadCommits(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "photo.jpg", catalogSourceHash, "image/jpeg")
	asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	ch := watchPhotoChanges(t, s)
	rejected := errors.New("rollback")
	mutate := func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE photo_files SET caption='River',revision=revision+1`)
		return err
	}
	require.ErrorIs(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error { require.NoError(t, mutate(tx)); return rejected }), rejected)
	require.Empty(t, ch)
	conn, err := s.writeDB.Conn(t.Context())
	require.NoError(t, err)
	// The next write uses a fresh connection, which must install its own capture triggers.
	require.ErrorIs(t, conn.Raw(func(any) error { return driver.ErrBadConn }), driver.ErrBadConn)
	require.NoError(t, s.withStorageTx(t.Context(), mutate))
	require.Equal(t, []PhotoEvent{{AssetID: asset.ID}}, nextPhotoWake(t, ch))
	// Two unread commits overflow the subscriber queue and require an ID-free refresh.
	require.NoError(t, s.withStorageTx(t.Context(), mutate))
	require.NoError(t, s.withStorageTx(t.Context(), mutate))
	require.Equal(t, []PhotoEvent{{}}, nextPhotoWake(t, ch))
	for _, statement := range []string{`UPDATE photo_files SET caption='Absent' WHERE 0`, `SELECT 1`} {
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error { _, err := tx.ExecContext(t.Context(), statement); return err }))
		require.Empty(t, ch)
	}
	t.Run("drops capture with the last subscriber", func(t *testing.T) {
		s := newTestStore(t)
		node := browsePhotoNode(t, s, "photo.jpg", catalogSourceHash, "image/jpeg")
		installed := func() int {
			conn, err := s.writeDB.Conn(t.Context())
			require.NoError(t, err)
			defer func() { require.NoError(t, conn.Close()) }()
			var count int
			require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_temp_master WHERE name LIKE 'photo_event%'`).Scan(&count))
			return count
		}
		write := func() {
			require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), `UPDATE photo_files SET caption='River',revision=revision+1 WHERE node_id=?`, node.ID)
				return err
			}))
		}
		_, stopFirst := s.SubscribePhotoEvents()
		_, stopSecond := s.SubscribePhotoEvents()
		write()
		require.NotZero(t, installed())
		stopFirst()
		write()
		require.NotZero(t, installed(), "another stream still watches")
		stopSecond()
		write()
		require.Zero(t, installed(), "the last stream took the capture triggers with it")
		// A later subscriber gets them back.
		watchPhotoChanges(t, s)
		write()
		require.NotZero(t, installed())
	})
	t.Run("a subscriber that joins mid-write refetches everything", func(t *testing.T) {
		s := newTestStore(t)
		node := browsePhotoNode(t, s, "photo.jpg", catalogSourceHash, "image/jpeg")
		var events <-chan []PhotoEvent
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			events = watchPhotoChanges(t, s)
			_, err := tx.Exec(`UPDATE nodes SET name='committed.jpg',revision=revision+1 WHERE id=?`, node.ID)
			return err
		}))
		require.Equal(t, []PhotoEvent{{}}, nextPhotoWake(t, events))
	})
}

func TestPhotoEventsCaptureSetupFailure(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "photo.jpg", catalogSourceHash, "image/jpeg")
	// Installation sees the table, but arming fails without its companion.
	_, err := s.writeDB.ExecContext(t.Context(), `CREATE TEMP TABLE photo_event_capture (id TEXT PRIMARY KEY, hidden_only INTEGER NOT NULL); INSERT INTO photo_event_capture VALUES('setup-marker',0)`)
	require.NoError(t, err)
	events := watchPhotoChanges(t, s)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		var id string
		// The failed setup's DELETE must have rolled back to the savepoint.
		require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT id FROM temp.photo_event_capture`).Scan(&id))
		require.Equal(t, "setup-marker", id)
		_, err := tx.ExecContext(t.Context(), `UPDATE photo_files SET caption='Committed after setup failure' WHERE node_id=?`, node.ID)
		return err
	}))
	require.Equal(t, []PhotoEvent{{}}, nextPhotoWake(t, events))
	var caption string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT caption FROM photo_files WHERE node_id=?`, node.ID).Scan(&caption))
	require.Equal(t, "Committed after setup failure", caption)
}

func TestPhotoEventsCaptureDisarmFailure(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "photo.jpg", catalogSourceHash, "image/jpeg")
	asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	events := watchPhotoChanges(t, s)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE photo_files SET caption='Committed before disarm failure' WHERE node_id=?`, node.ID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(), `CREATE TEMP TABLE disarm_connection_marker (id INTEGER); DROP TABLE temp.photo_event_capture_active`)
		return err
	}))
	require.Equal(t, []PhotoEvent{{AssetID: asset.ID}}, nextPhotoWake(t, events))
	var caption string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT caption FROM photo_files WHERE node_id=?`, node.ID).Scan(&caption))
	require.Equal(t, "Committed before disarm failure", caption)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		var markers int
		require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_temp_master WHERE name='disarm_connection_marker'`).Scan(&markers))
		require.Zero(t, markers, "the armed connection was discarded")
		_, err := tx.ExecContext(t.Context(), `UPDATE photo_files SET caption='Committed on replacement connection' WHERE node_id=?`, node.ID)
		return err
	}))
	require.Equal(t, []PhotoEvent{{AssetID: asset.ID}}, nextPhotoWake(t, events))
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT caption FROM photo_files WHERE node_id=?`, node.ID).Scan(&caption))
	require.Equal(t, "Committed on replacement connection", caption)
}

func TestPhotoEventsNonPhotoFiltering(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "photo.jpg", catalogSourceHash, "image/jpeg")
	asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	ch := watchPhotoChanges(t, s)
	browsePhotoNode(t, s, "document.txt", fakeHash("66"), "text/plain")
	require.Empty(t, ch)
	for _, statement := range []string{
		`INSERT INTO storage_operations(operation_id,kind,request_version,request_digest,request_json,plan_json,state,created_at,updated_at) VALUES('op','copy',1,'digest','{}','{}','queued','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
		`UPDATE storage_operations SET cursor='next'`,
	} {
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error { _, err := tx.ExecContext(t.Context(), statement); return err }))
		require.Empty(t, ch, "queue tables are not photo changes")
	}
	ordinary := browsePhotoNode(t, s, "ordinary.bin", node.BlobHash, "application/octet-stream")
	require.Equal(t, []PhotoEvent{{}}, nextPhotoWake(t, ch))
	tag, err := s.CreateTag(t.Context(), "Synthetic duplicate keyword")
	require.NoError(t, err)
	nextPhotoWake(t, ch)
	ordinary, _, err = s.Move(t.Context(), ordinary.ID, s.RootID(), "renamed.bin", ordinary.Revision)
	require.NoError(t, err)
	require.Empty(t, ch)
	_, err = s.AssignTag(t.Context(), tag.ID, ordinary.ID, ordinary.Revision)
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{}}, nextPhotoWake(t, ch))
	_, _, err = s.Trash(t.Context(), ordinary.ID, UnconditionalRev)
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{AssetID: asset.ID}}, nextPhotoWake(t, ch))
	t.Run("folder renames", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		parent, err := s.Mkdir(ctx, s.RootID(), "Original folder")
		require.NoError(t, err)
		nested, err := s.Mkdir(ctx, parent.ID, "Nested")
		require.NoError(t, err)
		photo, err := s.CreateFile(ctx, nested.ID, "photo.jpg", catalogSourceHash, 20, "image/jpeg")
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(ctx, photo.ID)
		require.NoError(t, err)
		unrelated, err := s.Mkdir(ctx, s.RootID(), "Empty folder")
		require.NoError(t, err)
		events := watchPhotoChanges(t, s)
		_, _, err = s.Move(ctx, unrelated.ID, s.RootID(), "Renamed empty folder", unrelated.Revision)
		require.NoError(t, err)
		require.Empty(t, events)
		_, _, err = s.Move(ctx, parent.ID, s.RootID(), "Renamed photo folder", UnconditionalRev)
		require.NoError(t, err)
		require.Equal(t, []PhotoEvent{{AssetID: asset.ID}}, nextPhotoWake(t, events))
	})
}

func TestPhotoEventsOverflowCollapses(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for i := range PhotoEventIDLimit {
		browsePhotoNode(t, s, fmt.Sprintf("photo-%d.jpg", i), fakeHash("55"), "image/jpeg")
	}
	ch := watchPhotoChanges(t, s)
	touch := func(where string) {
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `UPDATE photo_files SET caption='River',revision=revision+1 `+where)
			return err
		}))
	}
	touch("")
	require.Len(t, nextPhotoWake(t, ch), PhotoEventIDLimit)
	browsePhotoNode(t, s, "extra.jpg", fakeHash("56"), "image/jpeg")
	nextPhotoWake(t, ch)
	touch("")
	require.Equal(t, []PhotoEvent{{}}, nextPhotoWake(t, ch))
	require.Equal(t, []PhotoEvent{{}}, CollapsePhotoEvents([]PhotoEvent{{AssetID: "a"}, {}}))
	require.Equal(t, []PhotoEvent{{AssetID: "a"}}, CollapsePhotoEvents([]PhotoEvent{{AssetID: "a"}, {AssetID: "a"}}))
}

func TestPhotoEventsHiddenTransition(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset := albumAsset(t, s, "transition.jpg")
	require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
	events := watchPhotoChanges(t, s)
	_, err := s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{}}, nextPhotoWake(t, events), "hiding asks every viewer to refetch")
}

func TestPhotoEventsVisibleAccess(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "hidden.jpg")
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	asset, err := s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	visible := albumAsset(t, s, "visible.jpg")
	events, err := s.VisiblePhotoEvents(ctx, []PhotoEvent{{AssetID: asset.ID}, {AssetID: visible.ID}})
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{AssetID: visible.ID}}, events)
	events, err = s.VisiblePhotoEvents(ctx, []PhotoEvent{{AssetID: asset.ID}})
	require.NoError(t, err)
	require.Empty(t, events)
	events, err = s.VisiblePhotoEvents(ctx, []PhotoEvent{{AssetID: "missing"}})
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{}}, events)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	events, err = s.VisiblePhotoEvents(unlocked, []PhotoEvent{{AssetID: asset.ID}})
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{AssetID: asset.ID}}, events)
}

// Each write commits separately so a second dependency cannot hide a missing mapping.
func photoCaptureWrite(t *testing.T, s *Store, events <-chan []PhotoEvent, want []PhotoEvent, statement string, args ...any) {
	t.Helper()
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), statement, args...)
		return err
	}))
	require.ElementsMatch(t, want, nextPhotoWake(t, events), statement)
}

func TestPhotoEventsFileAndNodeOwnership(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	first := browsePhotoNode(t, s, "first.jpg", catalogSourceHash, "image/jpeg")
	second := browsePhotoNode(t, s, "second.jpg", fakeHash("second-owner"), "image/jpeg")
	a, err := s.PhotoAssetForNode(t.Context(), first.ID)
	require.NoError(t, err)
	b, err := s.PhotoAssetForNode(t.Context(), second.ID)
	require.NoError(t, err)
	set, err := s.CreatePhotoSet(t.Context(), "Review")
	require.NoError(t, err)
	_, err = s.ChangePhotoSetMembers(t.Context(), set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: []string{a.ID}})
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), "Location")
	require.NoError(t, err)
	_, err = s.AssignTag(t.Context(), tag.ID, first.ID, UnconditionalRev)
	require.NoError(t, err)
	observation, err := s.AppendNodeProvenance(t.Context(), ProvenanceAppendInput{NodeID: first.ID, IfRevision: UnconditionalRev, SourceKind: "agent", SourceDescription: "Source observation", OriginalPath: "/synthetic/first.jpg"})
	require.NoError(t, err)
	identity, err := provenanceIdentity(metadataProvenance{NodeID: second.ID, IngestID: observation.Fact.IngestID, OriginalPath: "/synthetic/second.jpg"})
	require.NoError(t, err)
	sidecar, err := s.CreateFile(t.Context(), s.RootID(), "first.xmp", browseHash("sidecar-text"), 20, "text/plain")
	require.NoError(t, err)
	_, err = s.AttachPhotoFile(t.Context(), a.ID, a.Revision, sidecar.ID, PhotoRoleSidecar, &a.Files[0].ID)
	require.NoError(t, err)
	events := watchPhotoChanges(t, s)
	one := []PhotoEvent{{AssetID: a.ID}}
	photoCaptureWrite(t, s, events, one, `UPDATE photo_assets SET excluded_at='2026-01-01T00:00:00Z' WHERE asset_id=?`, a.ID)
	photoCaptureWrite(t, s, events, one, `DELETE FROM photo_set_members WHERE set_id=?`, set.ID)
	photoCaptureWrite(t, s, events, one, `INSERT OR REPLACE INTO photo_timeline(file_id,sort_key,day) SELECT file_id,'2026-01-02','2026-01-02' FROM photo_files WHERE node_id=?`, first.ID)
	photoCaptureWrite(t, s, events, one, `UPDATE nodes SET name='renamed.jpg' WHERE id=?`, first.ID)
	photoCaptureWrite(t, s, events, one, `DELETE FROM node_tags WHERE node_id=?`, first.ID)
	photoCaptureWrite(t, s, events, []PhotoEvent{{AssetID: b.ID}}, `INSERT INTO provenance(identity,node_id,ingest_id,original_path) VALUES(?,?,?,?)`, identity, second.ID, observation.Fact.IngestID, "/synthetic/second.jpg")
	// Text eligibility is reachable on an attached sidecar with a supported text MIME.
	photoCaptureWrite(t, s, events, one, `DELETE FROM text_searchable_versions WHERE version_id=?`, sidecar.CurrentVersionID)
	sidecar, _, err = s.ReplaceContent(t.Context(), sidecar.ID, UnconditionalRev, browseHash("updated-sidecar-text"), 20, "text/plain")
	require.NoError(t, err)
	require.Equal(t, one, nextPhotoWake(t, events))
	require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{BlobHash: sidecar.BlobHash, Extractor: "synthetic", ExtractorVersion: 1, Status: ExtractionOK, Text: "River"}))
	require.Equal(t, one, nextPhotoWake(t, events))
}

func TestPhotoEventsGlobalDefinitions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, err := s.CreatePhotoSet(ctx, "Album")
	require.NoError(t, err)
	_, err = s.CreateTag(ctx, "Keyword")
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	person, err := s.CreatePerson(ctx, "Example Person", "operator")
	require.NoError(t, err)
	other, err := s.CreatePerson(ctx, "Other Person", "operator")
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Saved photos", "", SavedQueryKindQuery, []byte(`{"filters":{"kinds":["photo"]}}`))
	require.NoError(t, err)
	run, err := s.BeginIngest(ctx, "cli", "Synthetic collection")
	require.NoError(t, err)
	_, _, err = s.IngestFile(ctx, run, s.RootID(), "note.txt", fakeHash("collection-note"), 4, "text/plain", "/synthetic/note.txt", "")
	require.NoError(t, err)
	_, err = s.SetCollectionLabel(ctx, run.ID(), 1, new("Collection"))
	require.NoError(t, err)
	_, err = s.SetPhotoSettings(ctx, 1, PhotoSettingsUpdate{Preference: new("image")})
	require.NoError(t, err)
	aliasSource, err := s.CreatePerson(ctx, "Earlier name", "operator")
	require.NoError(t, err)
	aliasTarget, err := s.CreatePerson(ctx, "Current name", "operator")
	require.NoError(t, err)
	_, err = s.MergePersons(ctx, aliasTarget.PersonID, aliasSource.PersonID, uuid.NewString(), aliasTarget.Revision, aliasSource.Revision)
	require.NoError(t, err)
	events := watchPhotoChanges(t, s)
	for _, statement := range []string{
		`UPDATE photo_sets SET name='Renamed album'`,
		`UPDATE photo_library_settings SET revision=revision+1`,
		`UPDATE tags SET name='Renamed keyword'`,
		`UPDATE tags SET revision=revision+1`,
		`UPDATE persons SET display_name='Renamed person' WHERE state='curated'`,
		`UPDATE saved_queries SET name='Renamed query'`,
		`UPDATE collection_labels SET revision=revision+1`,
	} {
		t.Run(statement, func(t *testing.T) { photoCaptureWrite(t, s, events, []PhotoEvent{{}}, statement) })
	}
	require.NoError(t, s.ChangePhotoHidden(ctx, "synthetic-passcode", "changed-passcode"))
	require.Equal(t, []PhotoEvent{{}}, nextPhotoWake(t, events))
	_, err = s.RetirePerson(ctx, other.PersonID, other.Revision)
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{}}, nextPhotoWake(t, events))
	photoCaptureWrite(t, s, events, []PhotoEvent{{}}, `UPDATE person_aliases SET surviving_person_id=? WHERE surviving_person_id=?`, person.PersonID, aliasTarget.PersonID)
	_, err = s.LinkExternalIdentity(ctx, PersonExternalIdentity{PersonID: person.PersonID,
		System: "msgvault", ArchiveID: "synthetic", UID: "uid-1", UIDKind: "vcard_uid", UIDState: "current"}, person.Revision)
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{}}, nextPhotoWake(t, events))
}

func TestPhotoEventsCurrentVersionSources(t *testing.T) {
	t.Parallel()
	s, _ := newRenditionCatalogFixture(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "portrait.jpg", catalogSourceHash, "image/jpeg")
	browsePhotoNode(t, s, "other.jpg", fakeHash("other-version"), "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .3, Height: .4}, "")
	require.NoError(t, err)
	person, err := s.CreatePerson(ctx, "Example Person", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: node.CurrentVersionID, PersonID: person.PersonID, Role: "custodian", Action: "assert", Revision: 1})
	require.NoError(t, err)
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(node), document.PhotoQualitySignals{Blur: 1, Framing: .5}))
	profile := catalogProcessingProfile(t, false)
	require.NoError(t, s.StageRenditionBuild(ctx, catalogRenditionBuild(s, profile)))
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: node.CurrentVersionID, BuildID: catalogBuildID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z"}
	require.NoError(t, publishRenditionForTest(t, s, attachment, "2026-08-22T10:01:00.000000000Z", testSHA256([]byte("photo-capture-generation"))))
	request := renditionJobTestRequest(node.CurrentVersionID, profile)
	grantRenditionJobConsent(t, s, request)
	_, _, err = s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	events := watchPhotoChanges(t, s)
	candidate := candidateForTest(t, "face:"+face.FaceID, "Example Person", "", []PersonCandidateOccurrence{{
		ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: face.FaceID,
	}})
	candidate.Reason = "face_match"
	openCandidateForTest(t, s, candidate)
	require.Equal(t, []PhotoEvent{{AssetID: asset.ID}}, nextPhotoWake(t, events))
	faces, _, err := s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 1)
	require.False(t, faces[0].Dismissed)
	photoCaptureWrite(t, s, events, []PhotoEvent{{AssetID: asset.ID}}, `UPDATE person_match_candidates SET state='rejected',revision=revision+1,decided_at=? WHERE actor_key=?`, nowRFC3339(), candidate.ActorKey)
	faces, _, err = s.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.True(t, faces[0].Dismissed)
	photoCaptureWrite(t, s, events, []PhotoEvent{{AssetID: asset.ID}}, `UPDATE photo_faces SET revision=revision+1 WHERE face_id=?`, face.FaceID)
	photoCaptureWrite(t, s, events, []PhotoEvent{{AssetID: asset.ID}}, `UPDATE rendition_jobs SET phase='provider',provider_started=1 WHERE job_id IN (SELECT job_id FROM rendition_job_waiters WHERE content_version_id=?)`, node.CurrentVersionID)
	canonical := photoCanonical(t, photoMetadataField("image.exif.camera_make", "image.exif", "Make", photoString("Synthetic Camera")))
	generation, err := s.PublishSourceMetadata(ctx, node.BlobHash, browseHash("camera-extractor"), canonical)
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{AssetID: asset.ID}}, nextPhotoWake(t, events))
	metadata, err := s.ContentVersionPhotoMetadata(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, "Synthetic Camera", *metadata.Fields.CameraMake)
	// Projection rebuild deletes and restores rows while their source head remains selected.
	photoCaptureWrite(t, s, events, []PhotoEvent{{AssetID: asset.ID}}, `DELETE FROM photo_technical_metadata WHERE generation_id=?`, generation.GenerationID)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return insertPhotoTechnicalMetadataTx(ctx, tx, generation.GenerationID, metadata.Fields)
	}))
	require.Equal(t, []PhotoEvent{{AssetID: asset.ID}}, nextPhotoWake(t, events))
	otherGeneration, err := s.PublishSourceMetadata(ctx, node.BlobHash, browseHash("other-camera-extractor"), canonical)
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{AssetID: asset.ID}}, nextPhotoWake(t, events))
	photoCaptureWrite(t, s, events, []PhotoEvent{{AssetID: asset.ID}}, `UPDATE source_metadata_heads SET generation_id=? WHERE source_sha256=?`, generation.GenerationID, otherGeneration.SourceSHA256)
	preview := readyVisualPreview(t, node.BlobHash, browseHash("capture-preview"), 9)
	_, err = s.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, preview, &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{AssetID: asset.ID}}, nextPhotoWake(t, events))
	// Publishing an already recorded generation changes only the head.
	_, err = s.PublishVisualPreview(ctx, node.CurrentVersionID, preview, &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{AssetID: asset.ID}}, nextPhotoWake(t, events))
	for _, table := range []string{"photo_quality_signals", "person_document_assertions", "rendition_heads", "rendition_job_waiters"} {
		t.Run(table, func(t *testing.T) {
			photoCaptureWrite(t, s, events, []PhotoEvent{{AssetID: asset.ID}}, "DELETE FROM "+table+" WHERE content_version_id=?", node.CurrentVersionID)
		})
	}
}

func TestPhotoEventsDirectoryVisibility(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"move", "trash", "queued trash", "purge"} {
		for _, scope := range []string{"hidden", "visible", "mixed"} {
			counts := []int{1, PhotoEventIDLimit + 1, PhotoEventIDLimit + 2}
			if scope == "mixed" {
				counts[0] = 2
			}
			for _, count := range counts {
				t.Run(fmt.Sprintf("%s/%s/%d", action, scope, count), func(t *testing.T) {
					s := newTestStore(t)
					ctx := t.Context()
					parent, err := s.Mkdir(ctx, s.RootID(), "Photo folder")
					require.NoError(t, err)
					require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
					var all, publicIDs []PhotoEvent
					for i := range count {
						node, err := s.CreateFile(ctx, parent.ID, fmt.Sprintf("photo-%d.jpg", i), fakeHash(strconv.Itoa(i)), 20, "image/jpeg")
						require.NoError(t, err)
						asset, err := s.PhotoAssetForNode(ctx, node.ID)
						require.NoError(t, err)
						all = append(all, PhotoEvent{AssetID: asset.ID})
						if scope == "hidden" || scope == "mixed" && i < count-1 {
							_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
							require.NoError(t, err)
						} else {
							publicIDs = append(publicIDs, PhotoEvent{AssetID: asset.ID})
						}
					}
					token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
					require.NoError(t, err)
					unlocked := WithPhotoHiddenToken(ctx, token)
					public := albumAsset(t, s, "public.jpg")
					events := watchPhotoChanges(t, s)
					if action == "move" || action == "queued trash" {
						_, _, err = s.Move(unlocked, parent.ID, s.RootID(), "Renamed folder", UnconditionalRev)
						require.NoError(t, err)
					}
					if action != "move" {
						_, _, err = s.Trash(unlocked, parent.ID, UnconditionalRev)
						require.NoError(t, err)
					}
					if action == "purge" {
						nextPhotoWake(t, events)
						_, err = s.TrashEmpty(unlocked, 0, true)
						require.NoError(t, err)
						// Permanent deletion removes files but retains the asset's graph identity.
						asset, err := s.PhotoAssetByID(unlocked, all[0].AssetID)
						require.NoError(t, err)
						require.Empty(t, asset.Files)
					}
					batch := nextPhotoWake(t, events)
					require.LessOrEqual(t, len(batch), PhotoEventIDLimit)
					wantAll, wantPublic := all, publicIDs
					if count > PhotoEventIDLimit || action == "queued trash" {
						wantAll = []PhotoEvent{{}}
						if scope != "hidden" {
							wantPublic = []PhotoEvent{{}}
						}
					}
					visible, err := s.VisiblePhotoEvents(ctx, batch)
					require.NoError(t, err)
					require.ElementsMatch(t, wantPublic, visible, "locked viewers see only public changes")
					visible, err = s.VisiblePhotoEvents(unlocked, batch)
					require.NoError(t, err)
					require.ElementsMatch(t, wantAll, visible, "unlocked viewers see every affected asset")
					mixed := CollapsePhotoEvents(append(batch, PhotoEvent{AssetID: public.ID}))
					visible, err = s.VisiblePhotoEvents(ctx, mixed)
					require.NoError(t, err)
					require.NotEmpty(t, visible, "a public event still reaches locked viewers in a mixed batch")
				})
			}
		}
	}
}
