package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func watchPhotoChanges(t *testing.T, s *Store) <-chan struct{} {
	t.Helper()
	events, stop := s.SubscribePhotoEvents()
	t.Cleanup(stop)
	return events
}

// requirePhotoSignal consumes the pending signal.
func requirePhotoSignal(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatalf("missing photo change signal: %s", message)
	}
}

func execStorageTx(t *testing.T, s *Store, statement string, args ...any) {
	t.Helper()
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), statement, args...)
		return err
	}))
}

func TestPhotoEventsOneSignalPerCommit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	browsePhotoNode(t, s, "photo.jpg", catalogSourceHash, "image/jpeg")
	ch := watchPhotoChanges(t, s)
	rejected := errors.New("rollback")
	write := func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE photo_files SET caption='River',revision=revision+1`)
		return err
	}
	require.ErrorIs(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error { require.NoError(t, write(tx)); return rejected }), rejected)
	require.Empty(t, ch, "a rolled-back write publishes nothing")
	require.NoError(t, s.withStorageTx(t.Context(), write))
	requirePhotoSignal(t, ch, "the first committed write")
	require.NoError(t, s.withStorageTx(t.Context(), write))
	require.NoError(t, s.withStorageTx(t.Context(), write))
	requirePhotoSignal(t, ch, "two unread commits merge")
	for _, statement := range []string{`UPDATE photo_files SET caption='Absent' WHERE 0`, `SELECT 1`} {
		execStorageTx(t, s, statement)
		require.Empty(t, ch, statement)
	}
	t.Run("drops triggers with the last subscriber", func(t *testing.T) {
		s := newTestStore(t)
		node := browsePhotoNode(t, s, "photo.jpg", catalogSourceHash, "image/jpeg")
		installed := func() int {
			conn, err := s.writeDB.Conn(t.Context())
			require.NoError(t, err)
			defer func() { require.NoError(t, conn.Close()) }()
			var count int
			require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_temp_master WHERE name LIKE 'photo\_dirty%' ESCAPE '\'`).Scan(&count))
			return count
		}
		touch := func() {
			execStorageTx(t, s, `UPDATE photo_files SET caption='River',revision=revision+1 WHERE node_id=?`, node.ID)
		}
		_, stopFirst := s.SubscribePhotoEvents()
		_, stopSecond := s.SubscribePhotoEvents()
		touch()
		require.NotZero(t, installed())
		stopFirst()
		touch()
		require.NotZero(t, installed(), "another stream still watches")
		stopSecond()
		touch()
		require.Zero(t, installed(), "the last stream took the triggers with it")
		watchPhotoChanges(t, s)
		touch()
		require.NotZero(t, installed(), "a later subscriber gets them back")
	})
	t.Run("a subscriber that joins mid-write is told", func(t *testing.T) {
		s := newTestStore(t)
		node := browsePhotoNode(t, s, "photo.jpg", catalogSourceHash, "image/jpeg")
		var events <-chan struct{}
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			events = watchPhotoChanges(t, s)
			_, err := tx.Exec(`UPDATE nodes SET name='committed.jpg',revision=revision+1 WHERE id=?`, node.ID)
			return err
		}))
		requirePhotoSignal(t, events, "triggers were not armed for this write")
	})
}

func TestPhotoEventsSignalWhatPhotosRead(t *testing.T) {
	t.Parallel()
	s, _, documents, profile := collectionCoverageFixture(t, 2)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "photo.jpg", catalogSourceHash, "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	sidecar, err := s.CreateFile(ctx, s.RootID(), "photo.xmp", browseHash("sidecar-text"), 20, "text/plain")
	require.NoError(t, err)
	_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &asset.Files[0].ID)
	require.NoError(t, err)
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor."})
	events := watchPhotoChanges(t, s)
	for _, change := range []struct {
		name string
		do   func() error
	}{
		{"caption edit", func() error {
			_, err := editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: asset.Files[0].Revision, Patch: PhotoAuthoredPatch{Caption: new("River")}}})
			return err
		}},
		{"node rename", func() error {
			_, _, err := s.Move(ctx, node.ID, s.RootID(), "renamed.jpg", UnconditionalRev)
			return err
		}},
		{"keyword create", func() error { _, err := s.CreateTag(ctx, "Harbor"); return err }},
		{"suggestion dismissal", func() error {
			_, err := s.RespondPhotoSuggestion(ctx, proposal.ID, dismissalOf(ctx, t, s, proposal))
			return err
		}},
		{"sidecar text", func() error {
			return s.RecordExtraction(ctx, ExtractionResult{BlobHash: sidecar.BlobHash, Extractor: "synthetic", ExtractorVersion: 1, Status: ExtractionOK, Text: "River"})
		}},
		// A document build moves the shared search generation, which changes which photos a text query matches.
		// The first publication inserts the head; the second moves it.
		{"unrelated rendition head move", func() error {
			collectionCoveragePublish(t, s, documents[0], profile, "complete")
			requirePhotoSignal(t, events, "a new head")
			collectionCoveragePublish(t, s, documents[1], profile, "complete")
			return nil
		}},
	} {
		require.NoError(t, change.do(), change.name)
		requirePhotoSignal(t, events, change.name)
	}
}

func TestPhotoEventsIgnoreBookkeeping(t *testing.T) {
	t.Parallel()
	s, _, source := newPublishedVectorIndexFixture(t)
	now := time.Now().UTC()
	require.NoError(t, putVectorIndexGenerationForTest(t, s, vectorIndexGenerationFixture("active", source, []byte("index"), now)))
	execStorageTx(t, s, `INSERT OR IGNORE INTO text_extraction_queue(blob_hash,next_attempt_at) SELECT hash,'2026-01-01T00:00:00Z' FROM blobs LIMIT 1`)
	events := watchPhotoChanges(t, s)
	for name, write := range map[string]func(){
		"queue tick": func() {
			execStorageTx(t, s, `UPDATE text_extraction_queue SET next_attempt_at='2026-01-02T00:00:00Z'`)
		},
		"reader lease": func() {
			_, err := s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "reader", now, time.Minute)
			require.NoError(t, err)
		},
		"storage operations": func() {
			execStorageTx(t, s, `INSERT INTO storage_operations(operation_id,kind,request_version,request_digest,request_json,plan_json,state,created_at,updated_at) VALUES('op','copy',1,'digest','{}','{}','queued','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
		},
	} {
		write()
		require.Empty(t, events, name)
	}
}

// A claim heartbeat moves the job's update time, which ranks which attempt a photo's coverage reports.
func TestPhotoEventsSignalRenditionHeartbeat(t *testing.T) {
	t.Parallel()
	s, _ := newRenditionCatalogFixture(t)
	node := browsePhotoNode(t, s, "portrait.jpg", catalogSourceHash, "image/jpeg")
	request := renditionJobTestRequest(node.CurrentVersionID, catalogProcessingProfile(t, false))
	grantRenditionJobConsent(t, s, request)
	job, _, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "photo-worker", at, time.Minute)
	require.NoError(t, err)
	events := watchPhotoChanges(t, s)
	_, err = s.RenewRenditionJobClaim(t.Context(), claim, at.Add(time.Second), time.Minute)
	require.NoError(t, err)
	requirePhotoSignal(t, events, "a heartbeat")
}

func TestPhotoEventsUnwatchedTablesExist(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for reason, tables := range photoUnwatchedTables {
		for _, table := range tables {
			var found bool
			require.NoError(t, s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_list WHERE schema='main' AND type='table' AND name=?)`, table).Scan(&found))
			require.True(t, found, "%s: %s", reason, table)
		}
	}
}
