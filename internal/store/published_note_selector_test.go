package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Removing the eligibility predicate, applying it after LIMIT, or expanding
// this collection into the exact-ID fence must fail this scale regression.
func TestPublishedNoteSelectorBeforeLimit4097(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	source := seedNotePublication(t, s, "present")
	tx, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for i := 1; i <= 4097; i++ {
		version := noteOperationID(t)
		id := source.Identity.NodeID + int64(i)
		name := fmt.Sprintf("note note %04d.md", i)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO nodes(id,parent_id,name,kind,current_version_id,revision,created_at,modified_at) SELECT ?,parent_id,?,'file',?,1,created_at,modified_at FROM nodes WHERE id=?`, id, name, version, source.Identity.NodeID)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO content_versions(version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,introduced_operation_id,transition_kind) SELECT ?,?,blob_hash,size,mime_type,recorded_at,1,?,'content_create' FROM content_versions WHERE version_id=?`, version, id, noteOperationID(t), source.Identity.ContentVersionID)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO published_note_sources(publication_name,source_ref,node_id,content_version_id,blob_hash,size,source_revision,presence,availability,accepted_at) VALUES('notebook',?,?,?,?,4,1,'absent','retained',?)`, name, id, version, source.Identity.Hash, nowRFC3339())
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	opts := SearchOptions{PublishedNotes: &PublishedNoteSelector{Publication: "notebook"}}
	hits, more, err := s.SearchPageWithOptions(t.Context(), "note", 1, opts)
	require.NoError(t, err)
	require.False(t, more)
	require.Len(t, hits, 1)
	require.Equal(t, source.Identity.ContentVersionID, hits[0].Node.CurrentVersionID)
	require.NotNil(t, hits[0].PublishedNote)
	require.Equal(t, "folder/note.md", hits[0].PublishedNote.SourceRef)
	fence, err := s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{Filters: &opts})
	require.NoError(t, err)
	require.Equal(t, []string{source.Identity.ContentVersionID}, fence.ContentVersionIDs)
	require.Equal(t, 1, fence.ObservedScopeCount)
	_, err = s.db.ExecContext(t.Context(), `UPDATE published_note_sources SET presence='present'`)
	require.NoError(t, err)
	count, err := liveSearchScopeCount(t.Context(), s.db, opts)
	require.NoError(t, err)
	require.Equal(t, 4098, count)
	_, err = s.ResolveProcessingSourceFence(t.Context(), ProcessingSourceFenceRequest{Filters: &opts})
	require.ErrorIs(t, err, ErrProcessingSourceFenceScopeTooLarge, "explicit fence cap remains unchanged")
	page, err := s.ListDocuments(t.Context(), DocumentCatalogQuery{PublishedNotes: opts.PublishedNotes, PageSize: 2}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.True(t, page.HasNext)
}

func TestPublishedNoteNilSelectorCompatibility(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedNotePublication(t, s, "absent")
	ordinary, _, err := s.SearchPageWithOptions(t.Context(), "note", 10, SearchOptions{})
	require.NoError(t, err)
	require.Len(t, ordinary, 1)
	require.Nil(t, ordinary[0].PublishedNote)
	scoped, _, err := s.SearchPageWithOptions(t.Context(), "note", 10, SearchOptions{PublishedNotes: &PublishedNoteSelector{}})
	require.NoError(t, err)
	require.Empty(t, scoped)
}

func TestPublishedNoteMovedArchiveIdentity(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	source := seedNotePublication(t, s, "present")
	node, err := s.NodeByID(t.Context(), source.Identity.NodeID)
	require.NoError(t, err)
	_, _, err = s.Move(t.Context(), node.ID, s.RootID(), "renamed.md", node.Revision)
	require.NoError(t, err)
	opts := SearchOptions{PublishedNotes: &PublishedNoteSelector{}}
	hits, _, err := s.SearchPageWithOptions(t.Context(), "renamed", 5, opts)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "/renamed.md", hits[0].Path)
	require.NotNil(t, hits[0].PublishedNote)
	require.Equal(t, "folder/note.md", hits[0].PublishedNote.SourceRef)
	page, err := s.ListDocuments(t.Context(), DocumentCatalogQuery{PublishedNotes: opts.PublishedNotes}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.NotNil(t, page.Items[0].PublishedNote)
	require.Equal(t, hits[0].PublishedNote, page.Items[0].PublishedNote)
	node, err = s.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, node.BlobHash, node.Size, node.MimeType)
	require.NoError(t, err)
	hits, _, err = s.SearchPageWithOptions(t.Context(), "renamed", 5, opts)
	require.NoError(t, err)
	require.Empty(t, hits, "equal bytes with a new manual version are ineligible")
}

func TestPublishedNoteSelectorCursorRevision(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedNotePublication(t, s, "present")
	page, err := s.ListDocuments(t.Context(), DocumentCatalogQuery{PublishedNotes: &PublishedNoteSelector{}}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.NotEmpty(t, page.Query.PublishedNotesRevision)
	_, err = s.db.ExecContext(t.Context(), `UPDATE note_publications SET namespace_revision=namespace_revision+1`)
	require.NoError(t, err)
	_, err = s.ListDocuments(t.Context(), page.Query, nil, DocumentCatalogTraversalNext)
	require.ErrorIs(t, err, ErrInvalidDocumentCursor)
	for _, name := range []string{"UPPER", "../notes", "a/b"} {
		_, err = s.NormalizeSearchOptions(t.Context(), SearchOptions{PublishedNotes: &PublishedNoteSelector{Publication: name}})
		require.Error(t, err)
	}
}

func TestPublishedNotePageReadinessBindsSnapshot(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedNotePublication(t, s, "present")
	page, err := s.ListDocuments(t.Context(), DocumentCatalogQuery{PublishedNotes: &PublishedNoteSelector{Publication: "notebook"}}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.NotNil(t, page.NoteReadiness)
	require.Equal(t, "notebook", page.NoteReadiness.Publisher.Name)
	require.Equal(t, page.Items[0].PublishedNote.NamespaceRevision, page.NoteReadiness.Publisher.NamespaceRevision)
	require.Equal(t, "pending", page.NoteReadiness.ParseState)
	require.Empty(t, page.NoteReadiness.Generation)
	unscoped, err := s.ListDocuments(t.Context(), DocumentCatalogQuery{}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.Nil(t, unscoped.NoteReadiness)
	missing, err := s.ListDocuments(t.Context(), DocumentCatalogQuery{PublishedNotes: &PublishedNoteSelector{Publication: "missing"}}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.Nil(t, missing.NoteReadiness)
}

func TestPublishedNotePageReadinessTracksCurrentGeneration(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	notes := noteIndexFixture(t, s, indexNoteSpec{ref: "note.md", body: "note", keys: []NoteLookupKey{{"path", "note.md"}, {"basename", "note"}}})
	generation := beginIndexFixture(t, s)
	stageIndexFixture(t, s, generation, notes)
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), generation, "note.md", nil))
	require.NoError(t, s.PublishNoteIndex(t.Context(), generation))
	query := DocumentCatalogQuery{PublishedNotes: &PublishedNoteSelector{Publication: "notebook"}}
	page, err := s.ListDocuments(t.Context(), query, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.NotNil(t, page.NoteReadiness)
	require.Equal(t, generation.ID, page.NoteReadiness.Generation)
	require.Equal(t, "ready", page.NoteReadiness.ParseState)
	_, err = s.db.ExecContext(t.Context(), `UPDATE note_publications SET namespace_revision=namespace_revision+1 WHERE name=$1`, "notebook")
	require.NoError(t, err)
	page, err = s.ListDocuments(t.Context(), query, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.NotNil(t, page.NoteReadiness)
	require.Empty(t, page.NoteReadiness.Generation)
}

func TestPublishedNotePageReadinessReportsParseLimit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	noteIndexFixture(t, s, indexNoteSpec{ref: "note.md", body: "note", terminal: "parser_limit"})
	page, err := s.ListDocuments(t.Context(), DocumentCatalogQuery{PublishedNotes: &PublishedNoteSelector{Publication: "notebook"}}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.NotNil(t, page.NoteReadiness)
	require.Equal(t, "limit", page.NoteReadiness.ParseState)
}

func TestPublishedNoteFinalRevalidation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	source := seedNotePublication(t, s, "present")
	candidate := SearchCandidateIdentity{NodeID: source.Identity.NodeID, ContentVersionID: source.Identity.ContentVersionID, Evidence: []SearchEvidenceIdentity{{NodeID: source.Identity.NodeID, ContentVersionID: source.Identity.ContentVersionID, Kind: "node_name", NodeName: "note.md"}}}
	opts := SearchOptions{PublishedNotes: &PublishedNoteSelector{}}
	result, err := s.RevalidateSearchCandidates(t.Context(), []SearchCandidateIdentity{candidate}, opts, "", "")
	require.NoError(t, err)
	require.Len(t, result.Candidates, 1)
	require.NotNil(t, result.Candidates[0].PublishedNote)
	require.Equal(t, "folder/note.md", result.Candidates[0].PublishedNote.SourceRef)
	_, err = s.db.ExecContext(t.Context(), `UPDATE published_note_sources SET presence='absent'`)
	require.NoError(t, err)
	result, err = s.RevalidateSearchCandidates(t.Context(), []SearchCandidateIdentity{candidate}, opts, "", "")
	require.NoError(t, err)
	require.Empty(t, result.Candidates)
}
