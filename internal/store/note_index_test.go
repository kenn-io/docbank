package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/notemarkdown"
)

type indexNoteSpec struct {
	ref, body, terminal string
	keys                []NoteLookupKey
}
type indexNoteFixture struct {
	source NotePublicationSource
	parse  NoteParseRecord
	keys   []NoteLookupKey
}

// Fixtures use real publication authority and canonical parser output. The
// authored keys and expected lookup results are supplied independently.
func noteIndexFixture(tb testing.TB, s *Store, specs ...indexNoteSpec) []indexNoteFixture {
	tb.Helper()
	scan := beginNoteScan(tb, s)
	items := make([]indexNoteFixture, len(specs))
	for i, spec := range specs {
		digest := sha256.Sum256([]byte(spec.body))
		request := noteUpload(tb, scan, spec.ref, hex.EncodeToString(digest[:]), 0)
		request.Size = int64(len(spec.body))
		items[i].source = acceptNote(tb, s, request).Source
		items[i].keys = spec.keys
		record := NoteParseRecord{ContentVersionID: items[i].source.Identity.ContentVersionID, Hash: request.Hash, Size: request.Size, Recipe: notemarkdown.Recipe}
		if spec.terminal != "pending" {
			if spec.terminal != "" {
				record.State, record.Reason = "limit", spec.terminal
			} else {
				note, err := notemarkdown.Parse(tb.Context(), []byte(spec.body))
				require.NoError(tb, err)
				record.Artifact, err = notemarkdown.CanonicalArtifact(tb.Context(), note)
				require.NoError(tb, err)
				sum := sha256.Sum256(record.Artifact)
				record.State, record.Checksum = "ready", hex.EncodeToString(sum[:])
			}
			require.NoError(tb, s.RecordNoteParse(tb.Context(), record))
		}
		items[i].parse = record
	}
	completeNoteScan(tb, s, scan)
	sources, err := s.EligibleNoteSources(tb.Context(), "notebook", "", 1000)
	require.NoError(tb, err)
	byRef := make(map[string]NotePublicationSource, len(sources))
	for _, source := range sources {
		byRef[source.Identity.SourceRef] = source
	}
	for i := range items {
		items[i].source = byRef[items[i].source.Identity.SourceRef]
	}
	return items
}

func beginIndexFixture(tb testing.TB, s *Store) NoteIndexGeneration {
	tb.Helper()
	status, err := s.NotePublicationStatus(tb.Context(), "notebook")
	require.NoError(tb, err)
	generation, err := s.BeginNoteIndex(tb.Context(), NoteIndexTarget{Name: "notebook", DatabaseEpoch: status.DatabaseEpoch, NamespaceRevision: status.NamespaceRevision}, notemarkdown.Recipe, notemarkdown.ResolverRecipe)
	require.NoError(tb, err)
	return generation
}

func stageIndexFixture(tb testing.TB, s *Store, g NoteIndexGeneration, notes []indexNoteFixture) {
	tb.Helper()
	for _, note := range notes {
		require.NoError(tb, s.StageNoteIndexSource(tb.Context(), g, note.source, note.parse, note.keys))
	}
	require.NoError(tb, s.SealNoteIndexKeys(tb.Context(), g))
}

func TestNoteIndexGenerationCAS(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"edit", "absence", "manual", "move", "restore", "policy"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			notes := noteIndexFixture(t, s, indexNoteSpec{ref: "note.md", body: "note", keys: []NoteLookupKey{{"path", "note.md"}, {"basename", "note"}}})
			g := beginIndexFixture(t, s)
			stageIndexFixture(t, s, g, notes)
			require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "note.md", nil))
			node, err := s.NodeByID(t.Context(), notes[0].source.Identity.NodeID)
			require.NoError(t, err)
			switch change {
			case "edit":
				scan := beginNoteScan(t, s)
				acceptNote(t, s, noteUpload(t, scan, "note.md", fakeHash("ab"), notes[0].source.Identity.SourceRevision))
			case "absence":
				completeNoteScan(t, s, beginNoteScan(t, s))
			case "manual":
				_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("ab"), 4, "text/markdown")
				require.NoError(t, err)
			case "move":
				_, _, err = s.Move(t.Context(), node.ID, s.RootID(), "moved.md", node.Revision)
				require.NoError(t, err)
			case "restore":
				var data bytes.Buffer
				require.NoError(t, s.ExportMetadata(t.Context(), &data))
				restored := newTestStore(t)
				require.NoError(t, restored.ImportMetadata(t.Context(), &data))
				s = restored
			case "policy":
				_, err = s.BeginNotePublicationScan(t.Context(), NotePublicationBeginRequest{Name: "notebook", Destination: "/notes", PolicyFingerprint: fakeHash("de"), OperationID: noteOperationID(t)})
				require.NoError(t, err)
			}
			require.ErrorIs(t, s.PublishNoteIndex(t.Context(), g), ErrNoteIndexStale)
			require.ErrorIs(t, s.SealNoteIndexKeys(t.Context(), g), ErrNoteIndexStale)
			_, err = s.NoteIndexCandidates(t.Context(), g, []NoteLookupKey{{"path", "note.md"}})
			require.ErrorIs(t, err, ErrNoteIndexStale)
			var heads int
			require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM note_index_heads`).Scan(&heads))
			require.Zero(t, heads)
		})
	}
}

func TestNoteIndexRejectsIncompleteBuild(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	notes := noteIndexFixture(t, s,
		indexNoteSpec{ref: "a.md", body: "note", keys: []NoteLookupKey{{"path", "a.md"}, {"basename", "a"}}},
		indexNoteSpec{ref: "b.md", body: "note", keys: []NoteLookupKey{{"path", "b.md"}, {"basename", "b"}}})
	g := beginIndexFixture(t, s)
	require.EqualValues(t, 2, g.ExpectedSources)
	require.NoError(t, s.StageNoteIndexSource(t.Context(), g, notes[0].source, notes[0].parse, notes[0].keys))
	require.ErrorIs(t, s.SealNoteIndexKeys(t.Context(), g), ErrNoteIndexIncomplete)
	_, err := s.NoteIndexCandidates(t.Context(), g, []NoteLookupKey{{"path", "a.md"}})
	require.ErrorIs(t, err, ErrNoteIndexIncomplete)
	require.ErrorIs(t, s.PublishNoteIndex(t.Context(), g), ErrNoteIndexIncomplete)
	require.ErrorIs(t, s.StageNoteIndexSource(t.Context(), g, notes[1].source, notes[1].parse, []NoteLookupKey{{"path", "b.md"}}), ErrInvalidNoteProjection)
	require.NoError(t, s.StageNoteIndexSource(t.Context(), g, notes[1].source, notes[1].parse, notes[1].keys))
	require.NoError(t, s.SealNoteIndexKeys(t.Context(), g))
	require.ErrorIs(t, s.StageNoteIndexSource(t.Context(), g, notes[1].source, notes[1].parse, notes[1].keys), ErrNoteIndexIncomplete)
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "a.md", nil))
	require.ErrorIs(t, s.PublishNoteIndex(t.Context(), g), ErrNoteIndexIncomplete)
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "b.md", nil))
	require.NoError(t, s.PublishNoteIndex(t.Context(), g))
	require.NoError(t, s.PublishNoteIndex(t.Context(), g), "a committed head replay is harmless")
	targets, err := s.NoteIndexTargets(t.Context(), "", 64)
	require.NoError(t, err)
	require.Empty(t, targets)
}

func TestNoteIndexEmptyAndAbandonedGenerations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	noteIndexFixture(t, s)
	g := beginIndexFixture(t, s)
	require.Zero(t, g.ExpectedSources)
	other := beginIndexFixture(t, s)
	require.NotEqual(t, g.ID, other.ID)
	require.ErrorIs(t, s.PublishNoteIndex(t.Context(), g), ErrNoteIndexStale)
	require.NoError(t, s.SealNoteIndexKeys(t.Context(), other))
	require.NoError(t, s.PublishNoteIndex(t.Context(), other))
	completeNoteScan(t, s, beginNoteScan(t, s))
	_, err := s.BeginNoteIndex(t.Context(), NoteIndexTarget{Name: other.Publication, DatabaseEpoch: other.DatabaseEpoch, NamespaceRevision: other.NamespaceRevision}, other.ParserRecipe, other.ResolverRecipe)
	require.ErrorIs(t, err, ErrNoteIndexCurrent, "quiet scans retain the graph generation")
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM note_index_generations`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestNoteIndexTerminalParseKeys(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	notes := noteIndexFixture(t, s,
		indexNoteSpec{ref: "terminal.md", body: "note", terminal: "parser_limit", keys: []NoteLookupKey{{"path", "terminal.md"}, {"basename", "terminal"}}},
		indexNoteSpec{ref: "pending.md", body: "note", terminal: "pending", keys: []NoteLookupKey{{"path", "pending.md"}, {"basename", "pending"}}})
	g := beginIndexFixture(t, s)
	require.NoError(t, s.StageNoteIndexSource(t.Context(), g, notes[0].source, notes[0].parse, notes[0].keys))
	badKeys := append(append([]NoteLookupKey{}, notes[0].keys...), NoteLookupKey{"alias", "invented"})
	require.ErrorIs(t, s.StageNoteIndexSource(t.Context(), g, notes[0].source, notes[0].parse, badKeys), ErrInvalidNoteProjection)
	require.ErrorIs(t, s.StageNoteIndexSource(t.Context(), g, notes[1].source, NoteParseRecord{}, notes[1].keys), ErrNoteIndexIncomplete)
	require.ErrorIs(t, s.SealNoteIndexKeys(t.Context(), g), ErrNoteIndexIncomplete, "an unparsed alias namespace cannot look complete")
}

func TestNoteIndexLookupEvidenceBounds(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	notes := noteIndexFixture(t, s,
		indexNoteSpec{ref: "a/note.md", body: "---\naliases: [shared, other]\n---\nnote", keys: []NoteLookupKey{{"path", "a/note.md"}, {"basename", "note"}, {"alias", "shared"}, {"alias", "other"}}},
		indexNoteSpec{ref: "b/note.md", body: "---\naliases: [shared, other]\n---\nnote", keys: []NoteLookupKey{{"path", "b/note.md"}, {"basename", "note"}, {"alias", "shared"}, {"alias", "other"}}},
		indexNoteSpec{ref: "c/note.md", body: "---\naliases: [shared]\n---\nnote", keys: []NoteLookupKey{{"path", "c/note.md"}, {"basename", "note"}, {"alias", "shared"}}})
	g := beginIndexFixture(t, s)
	stageIndexFixture(t, s, g, notes)
	rows, err := s.NoteIndexCandidates(t.Context(), g, []NoteLookupKey{{"alias", "shared"}, {"alias", "other"}})
	require.NoError(t, err)
	require.Len(t, rows, 2, "two bindings matching multiple keys must not duplicate evidence")
	require.Equal(t, "a/note.md", rows[0].SourceRef)
	require.Equal(t, "b/note.md", rows[1].SourceRef)
	for _, row := range rows {
		require.Len(t, row.Keys, 2)
		require.NotEmpty(t, row.Binding)
		require.LessOrEqual(t, len(row.Binding), 4096)
		require.Equal(t, "note", row.Kind)
	}
	rows, err = s.NoteIndexCandidates(t.Context(), g, []NoteLookupKey{{"path", "c/note.md"}})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "c/note.md", rows[0].SourceRef)
	_, err = s.NoteIndexCandidates(t.Context(), g, []NoteLookupKey{{"alias", "a"}, {"alias", "b"}, {"alias", "c"}, {"alias", "d"}, {"alias", "e"}})
	require.ErrorIs(t, err, ErrInvalidNoteProjection)
	_, err = s.NoteIndexCandidates(t.Context(), g, []NoteLookupKey{{"unknown", "a"}})
	require.ErrorIs(t, err, ErrInvalidNoteProjection)
}

func TestNoteIndexExactEdgeOccurrences(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	notes := noteIndexFixture(t, s,
		indexNoteSpec{ref: "source.md", body: "[[target#section]]", keys: []NoteLookupKey{{"path", "source.md"}, {"basename", "source"}}},
		indexNoteSpec{ref: "target.md", body: "# Section\n", keys: []NoteLookupKey{{"path", "target.md"}, {"basename", "target"}}})
	g := beginIndexFixture(t, s)
	stageIndexFixture(t, s, g, notes)
	edge := NoteIndexEdge{Ordinal: 0, Link: notemarkdown.Link{Span: notemarkdown.Span{Start: 0, End: 18}, DestinationSpan: notemarkdown.Span{Start: 2, End: 16}, Form: "wiki", RawTarget: "target#section", Target: "target#section"}, State: "resolved", Target: &notes[1].source.Identity, TargetHeading: &notemarkdown.Span{Start: 0, End: 10}}
	bad := edge
	bad.Link.Span.End--
	require.ErrorIs(t, s.StageNoteIndexEdges(t.Context(), g, "source.md", []NoteIndexEdge{bad}), ErrInvalidNoteProjection)
	bad = edge
	wrongTarget := *edge.Target
	wrongTarget.ContentVersionID = noteOperationID(t)
	bad.Target = &wrongTarget
	require.ErrorIs(t, s.StageNoteIndexEdges(t.Context(), g, "source.md", []NoteIndexEdge{bad}), ErrInvalidNoteProjection)
	require.ErrorIs(t, s.StageNoteIndexEdges(t.Context(), g, "source.md", nil), ErrNoteIndexIncomplete)
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "source.md", []NoteIndexEdge{edge}))
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "target.md", nil))
	require.NoError(t, s.PublishNoteIndex(t.Context(), g))
	var data []byte
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT edge_json FROM note_link_edges WHERE generation_id=$1 AND source_ref='source.md'`, g.ID).Scan(&data))
	var got NoteIndexEdge
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, edge, got)
}

func checkNoteIndexBackend(t *testing.T, s *Store) {
	t.Helper()
	notes := noteIndexFixture(t, s, indexNoteSpec{ref: "note.md", body: "note", keys: []NoteLookupKey{{"path", "note.md"}, {"basename", "note"}}})
	g := beginIndexFixture(t, s)
	stageIndexFixture(t, s, g, notes)
	rows, err := s.NoteIndexCandidates(t.Context(), g, []NoteLookupKey{{"basename", "note"}})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "note.md", rows[0].SourceRef)
	identity, record, err := s.NoteIndexSource(t.Context(), g, "note.md")
	require.NoError(t, err)
	require.Equal(t, notes[0].source.Identity, identity)
	require.Equal(t, notes[0].parse, record)
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "note.md", nil))
	require.NoError(t, s.PublishNoteIndex(t.Context(), g))
}

func checkNoteIndexSQLite(t *testing.T) {
	t.Helper()
	checkNoteIndexBackend(t, newTestStore(t))
}
func checkNoteIndexPostgres(t *testing.T) {
	t.Helper()
	checkNoteIndexBackend(t, newPostgresQueryStore(t))
}

func TestNoteIndexExtensionOnlySource(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	notes := noteIndexFixture(t, s, indexNoteSpec{ref: ".md", body: "note", keys: []NoteLookupKey{{"path", ".md"}}})
	g := beginIndexFixture(t, s)
	stageIndexFixture(t, s, g, notes)
	rows, err := s.NoteIndexCandidates(t.Context(), g, []NoteLookupKey{{"path", ".md"}})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, ".md", rows[0].SourceRef)
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, ".md", nil))
	require.NoError(t, s.PublishNoteIndex(t.Context(), g))
}

func TestNoteIndexEdgePassRollsBackAndReplays(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	notes := noteIndexFixture(t, s, indexNoteSpec{ref: "note.md", body: "[[one]] [[two]]", keys: []NoteLookupKey{{"path", "note.md"}, {"basename", "note"}}})
	g := beginIndexFixture(t, s)
	stageIndexFixture(t, s, g, notes)
	edges := []NoteIndexEdge{
		{Ordinal: 0, Link: notemarkdown.Link{Span: notemarkdown.Span{Start: 0, End: 7}, DestinationSpan: notemarkdown.Span{Start: 2, End: 5}, Form: "wiki", RawTarget: "one", Target: "one"}, State: "missing"},
		{Ordinal: 1, Link: notemarkdown.Link{Span: notemarkdown.Span{Start: 8, End: 15}, DestinationSpan: notemarkdown.Span{Start: 10, End: 13}, Form: "wiki", RawTarget: "two", Target: "two"}, State: "missing"},
	}
	bad := append([]NoteIndexEdge{}, edges...)
	bad[1].Ordinal = 9
	require.ErrorIs(t, s.StageNoteIndexEdges(t.Context(), g, "note.md", bad), ErrInvalidNoteProjection)
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM note_link_edges WHERE generation_id=$1`, g.ID).Scan(&count))
	require.Zero(t, count, "a partial edge pass must roll back")
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "note.md", edges))
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "note.md", edges))
	require.NoError(t, s.PublishNoteIndex(t.Context(), g))
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "note.md", edges))
	bad = append([]NoteIndexEdge{}, edges...)
	bad[1].State = "ambiguous"
	require.ErrorIs(t, s.StageNoteIndexEdges(t.Context(), g, "note.md", bad), ErrInvalidNoteProjection)
}

func TestNoteIndexTargetHeadingUsesHeadingSpan(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	notes := noteIndexFixture(t, s,
		indexNoteSpec{ref: "source.md", body: "[[target#first]]", keys: []NoteLookupKey{{"path", "source.md"}, {"basename", "source"}}},
		indexNoteSpec{ref: "target.md", body: "# First\nparagraph\n\n# Second\n", keys: []NoteLookupKey{{"path", "target.md"}, {"basename", "target"}}})
	g := beginIndexFixture(t, s)
	stageIndexFixture(t, s, g, notes)
	edge := NoteIndexEdge{Ordinal: 0, Link: notemarkdown.Link{Span: notemarkdown.Span{Start: 0, End: 16}, DestinationSpan: notemarkdown.Span{Start: 2, End: 14}, Form: "wiki", RawTarget: "target#first", Target: "target#first"}, State: "resolved", Target: &notes[1].source.Identity, TargetHeading: &notemarkdown.Span{Start: 0, End: 8}}
	require.NoError(t, s.StageNoteIndexEdges(t.Context(), g, "source.md", []NoteIndexEdge{edge}))
}
