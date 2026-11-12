package store

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/notemarkdown"
)

func TestNoteRawEncodingBoundsAndCancellation(t *testing.T) {
	t.Parallel()
	// {"<key>":0} adds six bytes; no top-level newline is admitted.
	key := strings.Repeat("a", maxNoteRawBytes-6)
	encoded, err := encodeNoteRaw(t.Context(), map[string]any{key: 0})
	require.NoError(t, err)
	require.Len(t, encoded, maxNoteRawBytes)
	_, err = encodeNoteRaw(t.Context(), map[string]any{key + "a": 0})
	require.ErrorIs(t, err, ErrNoteRawLimit)
	_, err = encodeNoteRaw(t.Context(), map[string]any{"escaped": strings.Repeat("\x01", maxNoteRawBytes/6)})
	require.ErrorIs(t, err, ErrNoteRawLimit, "escaping and container punctuation count toward the cap")
	for _, value := range []any{
		map[string]any{strings.Repeat("\x01", 1024): []any{"quote\"", "slash\\", "café"}},
		[]any{map[string]any{"": map[string]any{"nested": true}}, nil, -1},
		strings.Repeat("a", 1023) + "é" + strings.Repeat("\x01", 1025),
	} {
		encoded, err := encodeNoteRaw(t.Context(), map[string]any{"value": value})
		require.NoError(t, err)
		want, err := json.Marshal(map[string]any{"value": value})
		require.NoError(t, err)
		require.JSONEq(t, string(want), encoded)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = encodeNoteRaw(ctx, map[string]any{"key": "value"})
	require.ErrorIs(t, err, context.Canceled)
}

func TestNoteRawAliasExpansionPublishesNoPartialProjection(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	// 41 copies of a 700,000-byte key fit B's 32 MiB derived budget,
	// but exceed the independent 24 MiB persisted JSON budget.
	// Explicit YAML keys avoid the scanner's 1,024-character simple-key cap.
	src := "---\nbase: &m\n  ? " + strings.Repeat("a", 700000) + "\n  : 0\ncopies: [" + strings.Repeat("*m,", 39) + "*m]\n---\n# Readable\n"
	n := createNoteFixture(t, s, s.RootID(), "expanded.md", src, "text/markdown")
	note, err := notemarkdown.Parse(t.Context(), []byte(src), notemarkdown.Limits{})
	require.NoError(t, err)
	target := noteTargets(t, s, notemarkdown.Recipe)[0]
	require.ErrorIs(t, s.PublishNoteParse(t.Context(), target, notemarkdown.Recipe, "ok", note), ErrNoteRawLimit)
	require.Equal(t, []int{0, 0, 0, 0, 0}, noteCatalogCounts(t, s, n.CurrentVersionID))
	require.NoError(t, s.PublishNoteParse(t.Context(), target, notemarkdown.Recipe, "limit", notemarkdown.Note{}))
	require.Equal(t, []int{1, 0, 0, 0, 0}, noteCatalogCounts(t, s, n.CurrentVersionID))
}
