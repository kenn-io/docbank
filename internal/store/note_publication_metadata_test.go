package store

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Seed authority directly: scan transactions have their own protocol tests.
func seedNotePublication(t *testing.T, s *Store, presence string) NotePublicationSource {
	t.Helper()
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "note.md", fakeHash("ab"), 4, "text/markdown",
		BlobPhysical{Encoding: "raw", StoredBytes: 4, PackEligible: true, Created: true})
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO note_publications
		(name,destination,policy_fingerprint,namespace_revision,active,last_complete_at,last_accepted_at)
		VALUES('notebook','/',?,3,1,?,?)`, fakeHash("cd"), nowRFC3339(), nowRFC3339())
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO published_note_sources
		(publication_name,source_ref,node_id,content_version_id,blob_hash,size,source_revision,presence,availability,accepted_at)
		VALUES('notebook','folder/note.md',?,?,?,?,2,?,'retained',?)`, node.ID, node.CurrentVersionID, node.BlobHash, node.Size, presence, nowRFC3339())
	require.NoError(t, err)
	return NotePublicationSource{Identity: PublishedNoteIdentity{
		VaultID: s.VaultID(), Publication: "notebook", SourceRef: "folder/note.md", NodeID: node.ID,
		ContentVersionID: node.CurrentVersionID, Hash: node.BlobHash, Size: node.Size, SourceRevision: 2, NamespaceRevision: 3,
	}, Presence: presence, Availability: "retained"}
}

func TestNotePublicationMetadataRoundTrip(t *testing.T) {
	t.Parallel()
	source := newTestStore(t)
	accepted := seedNotePublication(t, source, "absent")
	before, err := source.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	require.NotEmpty(t, before.DatabaseEpoch)
	var snapshot bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &snapshot))
	require.Contains(t, snapshot.String(), `"type":"note_publication"`)
	require.Contains(t, snapshot.String(), `"type":"published_note_source"`)
	require.NotContains(t, snapshot.String(), before.DatabaseEpoch)
	var backup bytes.Buffer
	view, err := source.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	require.NoError(t, view.ExportBackup(t.Context(), &backup))
	require.NoError(t, view.Close())
	for _, data := range [][]byte{snapshot.Bytes(), backup.Bytes()} {
		restored := newTestStore(t)
		require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(data)))
		status, err := restored.NotePublicationStatus(t.Context(), "notebook")
		require.NoError(t, err)
		require.NotEqual(t, before.DatabaseEpoch, status.DatabaseEpoch)
		require.EqualValues(t, 1, status.Absent)
		require.Zero(t, status.Present)
		require.Nil(t, status.ActiveScan)
		require.True(t, status.Incomplete, "restored membership is durable; scan freshness is not")
		var version, presence string
		require.NoError(t, restored.db.QueryRowContext(t.Context(), `SELECT content_version_id,presence
			FROM published_note_sources WHERE publication_name='notebook' AND source_ref='folder/note.md'`).Scan(&version, &presence))
		require.Equal(t, accepted.Identity.ContentVersionID, version)
		require.Equal(t, "absent", presence)
		var roundTrip bytes.Buffer
		require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
		require.Equal(t, snapshot.Bytes(), roundTrip.Bytes())
	}
}

func TestNotePublicationMetadataRejectsInvalidAuthorityAtomically(t *testing.T) {
	t.Parallel()
	source := newTestStore(t)
	accepted := seedNotePublication(t, source, "present")
	var snapshot bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &snapshot))
	for _, mutation := range []struct{ kind, old, replacement string }{
		{"note_publication", `"name":"notebook"`, `"name":"INVALID"`},
		{"published_note_source", `"source_ref":"folder/note.md"`, `"source_ref":"../escape.md"`},
		{"published_note_source", `"source_revision":2`, `"source_revision":-1`},
		{"note_publication", `"namespace_revision":3`, `"namespace_revision":0`},
		{"published_note_source", `"presence":"present"`, `"presence":"invented"`},
		{"published_note_source", `"availability":"retained"`, `"availability":"invented"`},
		{"published_note_source", `"content_version_id":"` + accepted.Identity.ContentVersionID + `"`, `"content_version_id":"` + metadataVersionOld + `"`},
		{"published_note_source", `"blob_hash":"` + accepted.Identity.Hash + `"`, `"blob_hash":"` + fakeHash("ef") + `"`},
	} {
		t.Run(mutation.replacement, func(t *testing.T) {
			lines := strings.Split(snapshot.String(), "\n")
			changed := false
			for i, line := range lines {
				if strings.HasPrefix(line, `{"type":"`+mutation.kind+`",`) {
					require.Contains(t, line, mutation.old)
					lines[i] = strings.Replace(line, mutation.old, mutation.replacement, 1)
					changed = true
				}
			}
			require.True(t, changed)
			malformed := strings.Join(lines, "\n")
			target := newTestStore(t)
			require.Error(t, target.ImportMetadata(t.Context(), strings.NewReader(malformed)))
			var count int
			require.NoError(t, target.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM note_publications`).Scan(&count))
			require.Zero(t, count, "failed restore must leave a pristine target")
		})
	}
}

func TestPublishedNoteValidation(t *testing.T) {
	valid := NotePublicationSource{Identity: PublishedNoteIdentity{VaultID: metadataIngestID,
		Publication: "notebook", SourceRef: "folder/note.md", NodeID: 2,
		ContentVersionID: metadataVersionCurrent, Hash: fakeHash("ab"), Size: 4, SourceRevision: 1, NamespaceRevision: 1,
	}, Presence: "unknown", Availability: "unavailable"}
	require.NoError(t, ValidatePublishedNoteSource(valid))
	for _, mutate := range []func(*NotePublicationSource){
		func(s *NotePublicationSource) { s.Identity.Publication = "INVALID" },
		func(s *NotePublicationSource) { s.Identity.SourceRef = "../escape" },
		func(s *NotePublicationSource) { s.Identity.Hash = "wrong" },
		func(s *NotePublicationSource) { s.Identity.NodeID = 0 },
		func(s *NotePublicationSource) { s.Identity.ContentVersionID = "wrong" },
		func(s *NotePublicationSource) { s.Identity.Size = -1 },
		func(s *NotePublicationSource) { s.Identity.SourceRevision = 0 },
		func(s *NotePublicationSource) { s.Identity.NamespaceRevision = -1 },
		func(s *NotePublicationSource) { s.Presence = "wrong" },
		func(s *NotePublicationSource) { s.Availability = "wrong" },
	} {
		invalid := valid
		mutate(&invalid)
		require.Error(t, ValidatePublishedNoteSource(invalid))
	}
}
