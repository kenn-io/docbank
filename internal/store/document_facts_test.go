package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDocumentFactsDatesState(t *testing.T) {
	s := newTestStore(t)
	version, _ := ingestDocumentEventTarget(t, s, "example.txt", "a1")
	facts, err := s.DocumentDateFacts(t.Context(), version.ID)
	require.NoError(t, err)
	require.Equal(t, "none", facts.State)
	require.Empty(t, facts.View.Events.Events)
	_, err = s.DocumentDateFacts(t.Context(), "00000000-0000-4000-8000-000000000000")
	require.ErrorIs(t, err, ErrNotFound)
}
