package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/notemarkdown"
)

func parsedNoteFixture(t testing.TB, s *Store) (NotePublicationSource, NoteParseRecord) {
	t.Helper()
	body := []byte("note")
	digest := sha256.Sum256(body)
	scan := beginNoteScan(t, s)
	source := acceptNote(t, s, noteUpload(t, scan, "note.md", hex.EncodeToString(digest[:]), 0)).Source
	note, err := notemarkdown.Parse(t.Context(), body)
	require.NoError(t, err)
	artifact, err := notemarkdown.CanonicalArtifact(t.Context(), note)
	require.NoError(t, err)
	sum := sha256.Sum256(artifact)
	return source, NoteParseRecord{ContentVersionID: source.Identity.ContentVersionID, Hash: note.SourceSHA256, Size: 4, Recipe: notemarkdown.Recipe, State: "ready", Checksum: hex.EncodeToString(sum[:]), Artifact: artifact}
}

func TestNoteParseExactVersionCache(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	source, record := parsedNoteFixture(t, s)
	_, found, err := s.NoteParseForVersion(t.Context(), record.ContentVersionID, record.Recipe)
	require.NoError(t, err)
	require.False(t, found)
	before, err := s.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	require.NoError(t, s.RecordNoteParse(t.Context(), record))
	got, found, err := s.NoteParseForVersion(t.Context(), record.ContentVersionID, record.Recipe)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, record, got)
	after, err := s.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	require.Greater(t, after.NamespaceRevision, before.NamespaceRevision)
	require.NoError(t, s.RecordNoteParse(t.Context(), record))
	quiet, err := s.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	require.Equal(t, after.NamespaceRevision, quiet.NamespaceRevision)
	node, err := s.NodeByID(t.Context(), source.Identity.NodeID)
	require.NoError(t, err)
	_, version, err := s.ReplaceContent(t.Context(), node.ID, node.Revision, record.Hash, record.Size, "text/markdown")
	require.NoError(t, err)
	_, found, err = s.NoteParseForVersion(t.Context(), version.ID, record.Recipe)
	require.NoError(t, err)
	require.False(t, found, "equal bytes do not borrow an exact-version binding")
	record.ContentVersionID = version.ID
	require.NoError(t, s.RecordNoteParse(t.Context(), record))
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM note_parse_generations`).Scan(&count))
	require.Equal(t, 1, count, "equal immutable bytes share a cache generation")
	for _, mutate := range []func(*NoteParseRecord){
		func(r *NoteParseRecord) { r.Hash = fakeHash("ab") }, func(r *NoteParseRecord) { r.Size++ }, func(r *NoteParseRecord) { r.Recipe = "unknown" }, func(r *NoteParseRecord) { r.Checksum = fakeHash("ab") }, func(r *NoteParseRecord) { r.State = "unknown" }, func(r *NoteParseRecord) { r.Artifact = bytes.Repeat([]byte("x"), 16*1024*1024+1) },
	} {
		bad := record
		mutate(&bad)
		require.Error(t, s.RecordNoteParse(t.Context(), bad))
	}
	// On-disk corruption is rejected on consumption, with the size checked before allocation.
	_, err = s.db.ExecContext(t.Context(), `UPDATE note_parse_generations SET artifact=?`, []byte(`{}`))
	require.NoError(t, err)
	_, _, err = s.NoteParseForVersion(t.Context(), record.ContentVersionID, record.Recipe)
	require.Error(t, err)
}

func TestNoteParseTerminalAndPending(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, record := parsedNoteFixture(t, s)
	record.State = "limit"
	record.Reason = "parser_limit"
	record.Artifact = nil
	record.Checksum = ""
	require.NoError(t, s.RecordNoteParse(t.Context(), record))
	got, found, err := s.NoteParseForVersion(t.Context(), record.ContentVersionID, record.Recipe)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, record, got)
	bad := record
	bad.Artifact = []byte(`{}`)
	require.Error(t, s.RecordNoteParse(t.Context(), bad))
	bad = record
	bad.Reason = "private arbitrary error"
	require.Error(t, s.RecordNoteParse(t.Context(), bad))
	targets, err := s.NoteIndexTargets(t.Context(), "", 64)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	sources, err := s.EligibleNoteSources(t.Context(), "notebook", "", 64)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	_, err = s.EligibleNoteSources(t.Context(), "notebook", "", 1001)
	require.Error(t, err)
}

func TestNoteProjectionMetadataOmitted(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, record := parsedNoteFixture(t, s)
	require.NoError(t, s.RecordNoteParse(t.Context(), record))
	var snapshot bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &snapshot))
	require.NotContains(t, snapshot.String(), record.Checksum)
	require.NotContains(t, snapshot.String(), "note_parse")
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), &snapshot))
	_, found, err := restored.NoteParseForVersion(t.Context(), record.ContentVersionID, record.Recipe)
	require.NoError(t, err)
	require.False(t, found)
	targets, err := restored.NoteIndexTargets(t.Context(), "", 64)
	require.NoError(t, err)
	require.Len(t, targets, 1)
}

func TestNoteProjectionPruneAndRestore(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	source, record := parsedNoteFixture(t, s)
	require.NoError(t, s.RecordNoteParse(t.Context(), record))
	node, err := s.NodeByID(t.Context(), source.Identity.NodeID)
	require.NoError(t, err)
	node, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("bc"), 4, "text/markdown")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(t.Context(), node.ID, node.Revision, VersionPruneSelector{KeepNewest: 1}, true)
	require.NoError(t, err)
	_, found, err := s.NoteParseForVersion(t.Context(), record.ContentVersionID, record.Recipe)
	require.NoError(t, err)
	require.False(t, found)
	require.Error(t, s.RecordNoteParse(t.Context(), record))
	var snapshot bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &snapshot))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), &snapshot))
	status, err := restored.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	require.EqualValues(t, 1, status.Unavailable)
}

func checkNoteProjectionSQLite(t *testing.T) {
	t.Parallel()
	checkNoteProjectionBackend(t, newTestStore(t))
}
func checkNoteProjectionPostgres(t *testing.T) {
	t.Parallel()
	checkNoteProjectionBackend(t, newPostgresQueryStore(t))
}
func checkNoteProjectionBackend(t *testing.T, s *Store) {
	t.Helper()
	_, record := parsedNoteFixture(t, s)
	require.NoError(t, s.RecordNoteParse(t.Context(), record))
	got, found, err := s.NoteParseForVersion(t.Context(), record.ContentVersionID, record.Recipe)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, record, got)
	sources, err := s.EligibleNoteSources(t.Context(), "notebook", "", 64)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	targets, err := s.NoteIndexTargets(t.Context(), "", 64)
	require.NoError(t, err)
	require.Len(t, targets, 1)
}

func TestNoteProjectionRejectsAlteredAuthorityLayout(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"note_publications", "published_note_sources", "note_publication_state"} {
		t.Run(table, func(t *testing.T) {
			s := newTestStore(t)
			_, err := s.db.ExecContext(t.Context(), "ALTER TABLE "+table+" ADD COLUMN synthetic_extra TEXT")
			require.NoError(t, err)
			blobs, err := queryTableColumns(t.Context(), s.db, "blobs")
			require.NoError(t, err)
			packs, err := queryTableColumns(t.Context(), s.db, "blob_packs")
			require.NoError(t, err)
			metadata, err := queryTableColumns(t.Context(), s.db, "vault_metadata")
			require.NoError(t, err)
			require.ErrorContains(t, validateCurrentSchemaColumns(t.Context(), s.driver, s.db, blobs, packs, metadata), "unexpected "+table+" layout")
		})
	}
}
