package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/notemarkdown"
)

func TestNoteContextCurrentGenerationAndStaleCursor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, notes := noteIndexFixture(t, s,
		indexNoteSpec{ref: "source.md", body: "[[target#section]]", keys: []NoteLookupKey{{"path", "source.md"}, {"basename", "source"}}},
		indexNoteSpec{ref: "target.md", body: "# Section\n", keys: []NoteLookupKey{{"path", "target.md"}, {"basename", "target"}}})
	g := beginIndexFixture(t, s)
	stageIndexFixture(t, s, g, notes)
	edge := NoteIndexEdge{Ordinal: 0, Link: notemarkdown.Link{Span: notemarkdown.Span{Start: 0, End: 18}, DestinationSpan: notemarkdown.Span{Start: 2, End: 16}, Form: "wiki", RawTarget: "target#section", Target: "target#section"}, State: "resolved", Target: &notes[1].source.Identity, TargetHeading: &notemarkdown.Span{Start: 0, End: 10}}
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "source.md", []NoteIndexEdge{edge}))
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "target.md", nil))
	require.NoError(t, s.PublishNoteIndex(t.Context(), g))
	request := NoteContextRequest{Publication: "notebook", SourceRef: "source.md", Direction: "outgoing", Limit: 1}
	page, err := s.NoteContext(t.Context(), request)
	require.NoError(t, err)
	require.True(t, page.GraphReady)
	require.Equal(t, notes[0].source.Identity.ContentVersionID, page.Source.Identity.ContentVersionID)
	require.Len(t, page.Edges, 1)
	require.Equal(t, edge, page.Edges[0].Edge)
	incoming, err := s.NoteContext(t.Context(), NoteContextRequest{Publication: "notebook", SourceRef: "target.md", Direction: "incoming", Limit: 10})
	require.NoError(t, err)
	require.Len(t, incoming.Edges, 1)
	require.Equal(t, "source.md", incoming.Edges[0].Source.SourceRef)
	request.ContentVersionID = noteOperationID(t)
	_, err = s.NoteContext(t.Context(), request)
	require.ErrorIs(t, err, ErrNoteGraphUnavailable)
	request.ContentVersionID = ""
	request.After = &NoteContextPosition{GenerationID: g.ID, DatabaseEpoch: g.DatabaseEpoch, NamespaceRevision: g.NamespaceRevision, SourceRef: "source.md", Ordinal: 0}
	completeNoteScan(t, s, beginNoteScan(t, s))
	_, err = s.NoteContext(t.Context(), request)
	require.ErrorIs(t, err, ErrNoteContextStale)
}

func TestNoteContextPagesPreserveEdgeOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	const body = "[[target]] [[target]]"
	_, notes := noteIndexFixture(t, s,
		indexNoteSpec{ref: "source.md", body: body, keys: []NoteLookupKey{{"path", "source.md"}, {"basename", "source"}}},
		indexNoteSpec{ref: "target.md", body: "target", keys: []NoteLookupKey{{"path", "target.md"}, {"basename", "target"}}})
	g := beginIndexFixture(t, s)
	stageIndexFixture(t, s, g, notes)
	note, err := notemarkdown.Parse(t.Context(), []byte(body))
	require.NoError(t, err)
	require.Len(t, note.Links, 2)
	edges := []NoteIndexEdge{{Ordinal: 0, Link: note.Links[0], State: "resolved", Target: &notes[1].source.Identity},
		{Ordinal: 1, Link: note.Links[1], State: "resolved", Target: &notes[1].source.Identity}}
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "source.md", edges))
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "target.md", nil))
	require.NoError(t, s.PublishNoteIndex(t.Context(), g))
	request := NoteContextRequest{Publication: "notebook", SourceRef: "source.md", Direction: "outgoing", Limit: 1}
	first, err := s.NoteContext(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, first.Edges, 1)
	require.EqualValues(t, 0, first.Edges[0].Edge.Ordinal)
	require.NotNil(t, first.Next)
	request.After = first.Next
	second, err := s.NoteContext(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, second.Edges, 1)
	require.EqualValues(t, 1, second.Edges[0].Edge.Ordinal)
	require.Nil(t, second.Next)
}

func checkNoteContextBackend(t *testing.T, s *Store) {
	t.Helper()
	_, notes := noteIndexFixture(t, s, indexNoteSpec{ref: "note.md", body: "note", keys: []NoteLookupKey{{"path", "note.md"}, {"basename", "note"}}})
	g := beginIndexFixture(t, s)
	stageIndexFixture(t, s, g, notes)
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "note.md", nil))
	require.NoError(t, s.PublishNoteIndex(t.Context(), g))
	page, err := s.NoteContext(t.Context(), NoteContextRequest{Publication: "notebook", SourceRef: "note.md", Direction: "outgoing", Limit: 10})
	require.NoError(t, err)
	require.True(t, page.GraphReady)
	require.Equal(t, notes[0].source.Identity.ContentVersionID, page.Source.Identity.ContentVersionID)
	require.Empty(t, page.Edges)
}

func checkNoteContextSQLite(t *testing.T)   { checkNoteContextBackend(t, newTestStore(t)) }
func checkNoteContextPostgres(t *testing.T) { checkNoteContextBackend(t, newPostgresQueryStore(t)) }
