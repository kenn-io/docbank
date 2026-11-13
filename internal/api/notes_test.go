package api_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/notemarkdown"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
)

func TestNoteRoutesReadCurrentOriginalAndCollectionStatus(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Watches = []config.WatchConfig{{Name: "records", Destination: "/records/"}}
	})
	ctx := t.Context()
	dir, err := s.Mkdir(ctx, s.RootID(), "records")
	require.NoError(t, err)
	const src = "---\ntitle: Authored title\ntags: [blue, green]\n---\n# Heading\n"
	hash, size, err := s.Blobs.WriteContext(ctx, strings.NewReader(src))
	require.NoError(t, err)
	n, err := s.CreateFile(ctx, dir.ID, "note.MD", hash, size, "text/plain")
	require.NoError(t, err)
	resp, body := get(t, ts, "/api/v1/notes/by-path?path=/records/note.MD", nil)
	require.Equal(t, http.StatusConflict, resp.StatusCode, body)
	require.Contains(t, body, "note_pending")
	page, err := s.ScanNoteTargetsAfter(ctx, notemarkdown.Recipe, "", 25)
	require.NoError(t, err)
	targets := page.Targets
	require.Len(t, targets, 1)
	note, err := notemarkdown.Parse(ctx, []byte(src), notemarkdown.Limits{})
	require.NoError(t, err)
	require.NoError(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "ok", note))
	resp, body = get(t, ts, "/api/v1/notes/by-path?path=/records/note.MD", nil)
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
	require.Equal(t, jsontext.Value(`"Authored title"`), got.Frontmatter["title"])
	resp, body = get(t, ts, "/api/v1/notes/status", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var status api.NotesStatus
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	require.Len(t, status.Items, 2)
	require.Equal(t, "records", status.Items[0].Name)
	require.Equal(t, 1, status.Items[0].Parsed)
	require.Zero(t, status.Items[1].Parsed)
}

func TestNoteRoutesRejectUnparsedBytesAndNonNotes(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	for _, name := range []string{"bad.md", "plain.txt"} {
		_, err := s.CreateFile(ctx, s.RootID(), name, strings.Repeat("b", 64), 1, "text/plain")
		require.NoError(t, err)
	}
	page, err := s.ScanNoteTargetsAfter(ctx, notemarkdown.Recipe, "", 25)
	require.NoError(t, err)
	targets := page.Targets
	require.Len(t, targets, 1)
	require.NoError(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "not_utf8", notemarkdown.Note{}))
	resp, body := get(t, ts, "/api/v1/notes/by-path?path=/bad.md", nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	require.Contains(t, body, "note_not_utf8")
	require.Contains(t, body, "invalid UTF-8 or NUL")
	resp, body = get(t, ts, "/api/v1/notes/by-path?path=/plain.txt", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/notes/by-path", nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/notes/by-path?path=bad.md", nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	require.Contains(t, body, "must be absolute")
	resp, body = get(t, ts, "/api/v1/notes/status", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, `"last_error":"not_utf8"`)
}

func TestNoteRoutesPreserveExactFrontmatterAndDiscardDiagnostics(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, src, diagnostic string
	}{
		{"numbers", "---\nlarge: 9007199254740993\nsigned: -9223372036854775808\nunsigned: 18446744073709551615\nnested: {values: [9007199254740993, 18446744073709551615]}\n---\n# Readable\n", ""},
		{"malformed", "---\ntitle: [broken\n---\n# Readable\n", "invalid_frontmatter"},
		{"absent", "# Readable\n", ""},
		{"empty", "---\n---\n# Readable\n", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ts, s := newTestServer(t, nil)
			ctx := t.Context()
			hash, size, err := s.Blobs.WriteContext(ctx, strings.NewReader(test.src))
			require.NoError(t, err)
			_, err = s.CreateFile(ctx, s.RootID(), "note.md", hash, size, "text/markdown")
			require.NoError(t, err)
			page, err := s.ScanNoteTargetsAfter(ctx, notemarkdown.Recipe, "", 25)
			require.NoError(t, err)
			targets := page.Targets
			require.Len(t, targets, 1)
			note, err := notemarkdown.Parse(ctx, []byte(test.src), notemarkdown.Limits{})
			require.NoError(t, err)
			require.NoError(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "ok", note))
			resp, body := get(t, ts, "/api/v1/notes/by-path?path=/note.md", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var got api.NoteByPath
			require.NoError(t, json.Unmarshal([]byte(body), &got))
			require.Equal(t, "Readable", got.Title)
			require.Equal(t, 1, got.Headings)
			require.Equal(t, test.diagnostic, got.FrontmatterError)
			if test.diagnostic != "" {
				require.Nil(t, got.Frontmatter)
				require.NotContains(t, body, "broken")
			} else {
				require.NotContains(t, body, `"frontmatter_error"`)
			}
			if test.name == "numbers" {
				require.Equal(t, jsontext.Value(`9007199254740993`), got.Frontmatter["large"])
				require.Equal(t, jsontext.Value(`-9223372036854775808`), got.Frontmatter["signed"])
				require.Equal(t, jsontext.Value(`18446744073709551615`), got.Frontmatter["unsigned"])
				require.Equal(t, jsontext.Value(`{"values":[9007199254740993,18446744073709551615]}`), got.Frontmatter["nested"])
			}
			if test.name == "absent" {
				require.Nil(t, got.Frontmatter)
			}
			if test.name == "empty" {
				require.NotNil(t, got.Frontmatter)
				require.Empty(t, got.Frontmatter)
			}
		})
	}
}
