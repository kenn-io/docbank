package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/notemarkdown"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestNoteRoutesReadCurrentOriginalAndCollectionStatus(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Watches = []config.WatchConfig{{Name: "records", Destination: "/records"}, {Name: "nested", Destination: "/records/deep"}}
	})
	ctx := t.Context()
	dir, err := s.Mkdir(ctx, s.RootID(), "records")
	require.NoError(t, err)
	deep, err := s.Mkdir(ctx, dir.ID, "deep")
	require.NoError(t, err)
	const src = "---\ntitle: Authored title\ntags: [blue, green]\n---\n# Heading\n"
	hash, size, err := s.Blobs.WriteContext(ctx, strings.NewReader(src))
	require.NoError(t, err)
	n, err := s.CreateFile(ctx, deep.ID, "note.MD", hash, size, "text/plain")
	require.NoError(t, err)
	resp, body := get(t, ts, "/api/v1/notes/by-path?path=/records/deep/note.MD", nil)
	require.Equal(t, http.StatusConflict, resp.StatusCode, body)
	require.Contains(t, body, "note_pending")
	targets, err := s.MissingNoteTargetsAfter(ctx, notemarkdown.Recipe, "", 25)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	note, err := notemarkdown.Parse(ctx, []byte(src), notemarkdown.Limits{})
	require.NoError(t, err)
	require.NoError(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "ok", note))
	resp, body = get(t, ts, "/api/v1/notes/by-path?path=/records/deep/note.MD", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got api.NoteByPath
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Equal(t, n.ID, got.NodeID)
	require.Equal(t, n.CurrentVersionID, got.ContentVersionID)
	require.Equal(t, hash, got.SHA256)
	require.Equal(t, size, got.Size)
	require.Equal(t, "Authored title", got.Title)
	require.Equal(t, []string{"blue", "green"}, got.Tags)
	require.Equal(t, "not_watched", got.SourceState)
	require.Equal(t, 1, got.Headings)
	require.Equal(t, "Authored title", got.Frontmatter["title"])
	resp, body = get(t, ts, "/api/v1/notes/status", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var status api.NotesStatus
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	require.Len(t, status.Items, 3)
	byName := map[string]api.NoteWatchStatus{}
	for _, item := range status.Items {
		byName[item.Name] = item
	}
	require.Equal(t, 1, byName["nested"].Parsed)
	require.Zero(t, byName["records"].Parsed)
	require.Zero(t, byName[""].Parsed)
	_, _, err = s.Trash(ctx, dir.ID, store.UnconditionalRev)
	require.NoError(t, err)
	resp, body = get(t, ts, "/api/v1/notes/by-path?path=/records/deep/note.MD", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
}

func TestNoteRoutesRejectUnparsedBytesAndNonNotes(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	for _, name := range []string{"bad.md", "plain.txt"} {
		_, err := s.CreateFile(ctx, s.RootID(), name, strings.Repeat("b", 64), 1, "text/plain")
		require.NoError(t, err)
	}
	targets, err := s.MissingNoteTargetsAfter(ctx, notemarkdown.Recipe, "", 25)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.NoError(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "not_utf8", notemarkdown.Note{}))
	resp, body := get(t, ts, "/api/v1/notes/by-path?path=/bad.md", nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	require.Contains(t, body, "note_not_utf8")
	resp, body = get(t, ts, "/api/v1/notes/by-path?path=/plain.txt", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/notes/by-path", nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/notes/status", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, `"last_error":"not_utf8"`)
}
