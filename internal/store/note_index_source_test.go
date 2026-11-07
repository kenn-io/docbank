package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNoteIndexSourceIsOneExactStagedBinding(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, notes := noteIndexFixture(t, s, indexNoteSpec{ref: "note.md", body: "# Heading\n", keys: []NoteLookupKey{{"path", "note.md"}, {"basename", "note"}}})
	g := beginIndexFixture(t, s)
	require.NoError(t, s.StageNoteIndexSource(t.Context(), g, notes[0].source, notes[0].parse, notes[0].keys))
	_, _, err := s.NoteIndexSource(t.Context(), g, "note.md")
	require.ErrorIs(t, err, ErrNoteIndexIncomplete)
	require.NoError(t, s.SealNoteIndexKeys(t.Context(), g))
	id, record, err := s.NoteIndexSource(t.Context(), g, "note.md")
	require.NoError(t, err)
	require.Equal(t, notes[0].source.Identity, id)
	require.Equal(t, notes[0].parse, record)
	_, _, err = s.NoteIndexSource(t.Context(), g, "missing.md")
	require.ErrorIs(t, err, ErrInvalidNoteProjection)
	_, _, err = s.NoteIndexSource(t.Context(), g, "../escape.md")
	require.ErrorIs(t, err, ErrInvalidNoteProjection)
	completeNoteScan(t, s, beginNoteScan(t, s))
	_, _, err = s.NoteIndexSource(t.Context(), g, "note.md")
	require.ErrorIs(t, err, ErrNoteIndexStale)
}

func TestNoteIndexSourceTerminalRecord(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, notes := noteIndexFixture(t, s, indexNoteSpec{ref: "note.md", body: "note", terminal: "parser_limit", keys: []NoteLookupKey{{"path", "note.md"}, {"basename", "note"}}})
	g := beginIndexFixture(t, s)
	stageIndexFixture(t, s, g, notes)
	id, record, err := s.NoteIndexSource(t.Context(), g, "note.md")
	require.NoError(t, err)
	require.Equal(t, notes[0].source.Identity, id)
	require.Equal(t, "limit", record.State)
	require.Equal(t, "parser_limit", record.Reason)
	require.Empty(t, record.Artifact)
}
