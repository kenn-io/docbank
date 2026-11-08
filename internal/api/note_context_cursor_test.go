package api

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestNoteContextCursorBindsRequestAndExpires(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	service := newDocumentCursorTestService(&now)
	request := NoteContextRequest{Publication: "notebook", SourceRef: "one.md", Direction: "incoming", Coverage: &NoteCoverageSelection{Profile: "local"}}
	position := store.NoteContextPosition{GenerationID: "00000000-0000-4000-8000-000000000001", DatabaseEpoch: "00000000-0000-4000-8000-000000000002", NamespaceRevision: 3, SourceRef: "other.md", Ordinal: 4}
	version := "00000000-0000-4000-8000-000000000003"
	cursor, err := service.encodeNoteContextCursor(request, version, position)
	require.NoError(t, err)
	request.Cursor = cursor
	gotVersion, got, err := service.decodeNoteContextCursor(request)
	require.NoError(t, err)
	require.Equal(t, version, gotVersion)
	require.Equal(t, position, *got)
	changed := request
	changed.Direction = "outgoing"
	_, _, err = service.decodeNoteContextCursor(changed)
	require.ErrorIs(t, err, store.ErrNoteContextStale)
	changed = request
	changed.Cursor = strings.Repeat("x", MaxDocumentCursorBytes+1)
	_, _, err = service.decodeNoteContextCursor(changed)
	require.ErrorIs(t, err, store.ErrNoteContextStale)
	now = now.Add(documentCursorTTL)
	_, _, err = service.decodeNoteContextCursor(request)
	require.ErrorIs(t, err, store.ErrNoteContextStale)
}
