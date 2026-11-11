package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/notemarkdown"
)

func createNoteFixture(t *testing.T, s *Store, parent int64, name, src, mime string) Node {
	t.Helper()
	n, err := s.CreateFile(t.Context(), parent, name, fmt.Sprintf("%x", sha256.Sum256([]byte(src))), int64(len(src)), mime)
	require.NoError(t, err)
	return n
}

func publishNoteFixture(t *testing.T, s *Store, target NoteTarget, src, recipe string) {
	t.Helper()
	note, err := notemarkdown.Parse(t.Context(), []byte(src), notemarkdown.Limits{})
	require.NoError(t, err)
	require.NoError(t, s.PublishNoteParse(t.Context(), target, recipe, "ok", note))
}

func noteTargets(t *testing.T, s *Store, recipe string) []NoteTarget {
	t.Helper()
	targets, err := s.MissingNoteTargetsAfter(t.Context(), recipe, "", 100)
	require.NoError(t, err)
	return targets
}

func noteCatalogCounts(t *testing.T, s *Store, version string) []int {
	t.Helper()
	var counts []int
	for _, table := range []string{"note_parses", "note_fields", "note_headings", "note_links", "note_aliases"} {
		var count int
		require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table+` WHERE version_id=$1`, version).Scan(&count))
		counts = append(counts, count)
	}
	return counts
}

func checkNoteLifecycle(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	const src = "---\ntitle: Design\ntags: [blue, green]\naliases: [Guide, GUIDE]\n---\n# Opening\n[Next](../next.md)\n"
	dir, err := s.Mkdir(ctx, s.RootID(), "drafts")
	require.NoError(t, err)
	n := createNoteFixture(t, s, dir.ID, "Design.MARKDOWN", src, "text/plain")
	targets := noteTargets(t, s, notemarkdown.Recipe)
	require.Len(t, targets, 1)
	publishNoteFixture(t, s, targets[0], src, notemarkdown.Recipe)
	require.Equal(t, []int{1, 5, 1, 1, 1}, noteCatalogCounts(t, s, n.CurrentVersionID))
	require.Empty(t, noteTargets(t, s, notemarkdown.Recipe))
	view, err := s.NoteByPath(ctx, "/drafts/Design.MARKDOWN", notemarkdown.Recipe)
	require.NoError(t, err)
	require.Equal(t, "Design", view.Summary.Title)
	require.Equal(t, []string{"blue", "green"}, view.Summary.Tags)
	require.Equal(t, "not_watched", view.Summary.SourceState)
	require.Equal(t, 1, view.HeadingCount)
	var at string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT parsed_at FROM note_parses WHERE version_id=$1`, n.CurrentVersionID).Scan(&at))
	publishNoteFixture(t, s, targets[0], src, notemarkdown.Recipe)
	var replayAt string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT parsed_at FROM note_parses WHERE version_id=$1`, n.CurrentVersionID).Scan(&replayAt))
	require.Equal(t, at, replayAt)
	require.Len(t, noteTargets(t, s, "next-recipe"), 1)
	publishNoteFixture(t, s, targets[0], src, "next-recipe")
	stale, err := s.NoteByPath(ctx, view.Summary.Path, notemarkdown.Recipe)
	require.NoError(t, err)
	require.Equal(t, "pending", stale.State)
	const replacement = "# Updated\n"
	_, version, err := s.ReplaceContent(ctx, n.ID, UnconditionalRev, fmt.Sprintf("%x", sha256.Sum256([]byte(replacement))), int64(len(replacement)), "text/markdown")
	require.NoError(t, err)
	// A late publication for the prior version must not recreate its rows.
	removed, err := s.PruneNoteParses(ctx, 100)
	require.NoError(t, err)
	require.EqualValues(t, 1, removed)
	publishNoteFixture(t, s, targets[0], src, notemarkdown.Recipe)
	require.Equal(t, []int{0, 0, 0, 0, 0}, noteCatalogCounts(t, s, n.CurrentVersionID))
	targets = noteTargets(t, s, notemarkdown.Recipe)
	require.Len(t, targets, 1)
	require.Equal(t, version.ID, targets[0].VersionID)
	publishNoteFixture(t, s, targets[0], replacement, notemarkdown.Recipe)
	_, _, err = s.Trash(ctx, dir.ID, UnconditionalRev)
	require.NoError(t, err)
	_, err = s.NoteByPath(ctx, "/drafts/Design.MARKDOWN", notemarkdown.Recipe)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.PruneNoteParses(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, []int{0, 0, 0, 0, 0}, noteCatalogCounts(t, s, version.ID))
	_, _, err = s.Restore(ctx, dir.ID, UnconditionalRev)
	require.NoError(t, err)
	require.Len(t, noteTargets(t, s, notemarkdown.Recipe), 1)
}

func checkNoteMoveAndPresence(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	const src = "[Next](next.md)\n"
	dir, err := s.Mkdir(ctx, s.RootID(), "old")
	require.NoError(t, err)
	n := createNoteFixture(t, s, dir.ID, "first.md", src, "text/plain")
	target := noteTargets(t, s, notemarkdown.Recipe)[0]
	publishNoteFixture(t, s, target, src, notemarkdown.Recipe)
	_, _, err = s.Move(ctx, dir.ID, s.RootID(), "new", UnconditionalRev)
	require.NoError(t, err)
	targets := noteTargets(t, s, notemarkdown.Recipe)
	require.Len(t, targets, 1)
	require.Equal(t, "/new/first.md", targets[0].Path)
	publishNoteFixture(t, s, target, src, notemarkdown.Recipe)
	var resolved string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT target_path FROM note_links WHERE version_id=$1`, n.CurrentVersionID).Scan(&resolved))
	require.Equal(t, "/old/next.md", resolved, "stale placement cannot publish")
	publishNoteFixture(t, s, targets[0], src, notemarkdown.Recipe)
	require.Empty(t, noteTargets(t, s, notemarkdown.Recipe))
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT target_path FROM note_links WHERE version_id=$1`, n.CurrentVersionID).Scan(&resolved))
	require.Equal(t, "/new/next.md", resolved)
	_, _, err = s.Move(ctx, n.ID, dir.ID, "renamed.md", UnconditionalRev)
	require.NoError(t, err)
	view, err := s.NoteByPath(ctx, "/new/renamed.md", notemarkdown.Recipe)
	require.NoError(t, err)
	require.Equal(t, "renamed", view.Summary.Title)
	// A Markdown suffix may disappear without changing the content version.
	_, _, err = s.Move(ctx, n.ID, dir.ID, "renamed.txt", UnconditionalRev)
	require.NoError(t, err)
	_, err = s.PruneNoteParses(ctx, 100)
	require.NoError(t, err)
	require.Empty(t, noteTargets(t, s, notemarkdown.Recipe))
	require.Equal(t, []int{0, 0, 0, 0, 0}, noteCatalogCounts(t, s, n.CurrentVersionID))
	_, _, err = s.Move(ctx, n.ID, dir.ID, "renamed.md", UnconditionalRev)
	require.NoError(t, err)
	require.Len(t, noteTargets(t, s, notemarkdown.Recipe), 1)
	// Source state uses the ordinary watch cursor and absence observation.
	run, err := s.BeginIngest(ctx, "watch", "inbox")
	require.NoError(t, err)
	watched, err := s.IngestFileExact(ctx, run, s.RootID(), "watched.md", fakeHash("a1"), 5, "text/markdown", "watched.md", "")
	require.NoError(t, err)
	pending, err := s.NoteByPath(ctx, "/watched.md", notemarkdown.Recipe)
	require.NoError(t, err)
	require.Equal(t, "present", pending.Summary.SourceState)
	require.NoError(t, s.ReconcileWatchSourcePresence(ctx, "inbox", WatchPresenceScan{ListedDirs: []string{""}}, time.Now()))
	view, err = s.NoteByPath(ctx, "/watched.md", notemarkdown.Recipe)
	require.NoError(t, err)
	require.Equal(t, "gone", view.Summary.SourceState)
	require.Equal(t, watched.ID, view.Summary.NodeID)
}

func checkNoteFailureAndValidation(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	for _, name := range []string{"a.MD", "b.mArKdOwN", "mime.bin", "plain.txt"} {
		mime := "text/plain"
		if name == "mime.bin" {
			mime = "text/markdown"
		}
		createNoteFixture(t, s, s.RootID(), name, "body", mime)
	}
	require.Len(t, noteTargets(t, s, notemarkdown.Recipe), 3)
	for i, target := range noteTargets(t, s, notemarkdown.Recipe) {
		state := []string{"not_utf8", "limit", "ok"}[i]
		note := notemarkdown.Note{}
		if state == "ok" {
			note.Lines = 1
		}
		require.NoError(t, s.PublishNoteParse(ctx, target, notemarkdown.Recipe, state, note))
	}
	oversize, err := s.CreateFile(ctx, s.RootID(), "large.md", fakeHash("b2"), MaxNoteBytes+1, "text/plain")
	require.NoError(t, err)
	targets := noteTargets(t, s, notemarkdown.Recipe)
	require.Len(t, targets, 1)
	require.Equal(t, oversize.CurrentVersionID, targets[0].VersionID)
	require.NoError(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "too_large", notemarkdown.Note{}))
	_, err = s.NoteByPath(ctx, "/large.md", notemarkdown.Recipe)
	require.ErrorIs(t, err, ErrNotFound)
	status, err := s.NotesStatus(ctx, notemarkdown.Recipe, nil)
	require.NoError(t, err)
	require.Len(t, status, 1)
	require.Equal(t, 1, status[0].Parsed)
	require.Equal(t, 3, status[0].Failed)
	require.Equal(t, 0, status[0].Pending)
	require.Equal(t, "too_large", status[0].LastError)
	require.Error(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "unknown", notemarkdown.Note{}))
	require.Error(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "ok", notemarkdown.Note{Lines: 1}))
	require.Error(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "limit", notemarkdown.Note{Title: "partial"}))
	revision := s.WriteRevision()
	_, err = s.PruneNoteParses(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, revision, s.WriteRevision())
}

func checkNoteFiveThousand(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	const src = "# Synthetic\n"
	// Seed source identities in one ordinary storage transaction. The tested
	// producer is PublishNoteParse, exercised once for every native version.
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for i := range 5000 {
			if _, _, err := s.createFileTx(ctx, tx, s.RootID(), fmt.Sprintf("note-%04d.md", i), fakeHash("c3"), int64(len(src)), "text/markdown"); err != nil {
				return err
			}
		}
		return nil
	}))
	cursor := ""
	published := 0
	for {
		targets, err := s.MissingNoteTargetsAfter(ctx, notemarkdown.Recipe, cursor, 100)
		require.NoError(t, err)
		if len(targets) == 0 {
			break
		}
		for _, target := range targets {
			publishNoteFixture(t, s, target, src, notemarkdown.Recipe)
			published++
		}
		cursor = targets[len(targets)-1].VersionID
	}
	require.Equal(t, 5000, published)
	status, err := s.NotesStatus(ctx, notemarkdown.Recipe, nil)
	require.NoError(t, err)
	require.Equal(t, 5000, status[0].Parsed)
	require.Zero(t, status[0].Pending+status[0].Failed)
	var count int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM note_headings`).Scan(&count))
	require.Equal(t, 5000, count)
}

func TestNoteEligibilityAndCollection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, mime, kind string
		want             bool
	}{
		{"a.MD", "text/plain", "file", true}, {"a.Markdown", "", "file", true}, {"opaque", "text/markdown", "file", true},
		{"a.txt", "text/plain", "file", false}, {"a.md", "text/markdown", "dir", false},
	} {
		require.Equal(t, test.want, IsNoteCandidate(Node{Name: test.name, MimeType: test.mime, Kind: test.kind}))
	}
	trash := "2026-01-01T00:00:00Z"
	require.False(t, IsNoteCandidate(Node{Name: "a.md", Kind: "file", TrashedAt: &trash}))
	watches := []NoteWatch{{"outer", "/work"}, {"inner", "/work/deep"}, {"root", "/"}}
	require.Equal(t, watches[1], NoteCollection("/work/deep/n.md", watches))
	require.Equal(t, watches[0], NoteCollection("/work/n.md", watches))
	require.Equal(t, watches[2], NoteCollection("/workshop/n.md", watches))
	require.Equal(t, NoteWatch{Destination: "/"}, NoteCollection("/n.md", nil))
}

func TestNoteLinkProjectionKeepsLiteralNamesAndExternalURIs(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ target, form, wantPath, wantKey string }{
		{"A:notes.md", "markdown", "/notes/A:notes.md", ""},
		{"../other.md", "markdown", "/other.md", ""},
		{"https://example.test/guide.md", "markdown", "", ""},
		{"mailto:reader@example.test", "markdown", "", ""},
		{"Meeting.v2", "wikilink", "", "meeting.v2"},
		{"Meeting.v2.MD", "wikilink", "", "meeting.v2"},
		{"", "wikilink", "/notes/current.md", ""},
	} {
		resolved, key := noteLinkIdentity("/notes/current.md", test.form, test.target)
		if test.wantPath == "" {
			require.Nil(t, resolved)
		} else {
			require.NotNil(t, resolved)
			require.Equal(t, test.wantPath, *resolved)
		}
		if test.wantKey == "" {
			require.Nil(t, key)
		} else {
			require.NotNil(t, key)
			require.Equal(t, test.wantKey, *key)
		}
	}
}

func TestNoteMetadataRestoreRebuildsWithoutDerivedJSONL(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	const src = "---\ntags: [blue]\n---\n# Title\n"
	n := createNoteFixture(t, s, s.RootID(), "note.md", src, "text/markdown")
	target := noteTargets(t, s, notemarkdown.Recipe)[0]
	publishNoteFixture(t, s, target, src, notemarkdown.Recipe)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	require.NotContains(t, exported.String(), `"type":"note_`)
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	targets := noteTargets(t, restored, notemarkdown.Recipe)
	require.Len(t, targets, 1)
	require.Equal(t, n.CurrentVersionID, targets[0].VersionID)
	require.Equal(t, []int{0, 0, 0, 0, 0}, noteCatalogCounts(t, restored, n.CurrentVersionID))
	publishNoteFixture(t, restored, targets[0], src, notemarkdown.Recipe)
	view, err := restored.NoteByPath(ctx, "/note.md", notemarkdown.Recipe)
	require.NoError(t, err)
	require.Equal(t, "Title", view.Summary.Title)
}

func TestUpgradeReleasedV0170RebuildsNoteCatalog(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			ctx := t.Context()
			dbPath := filepath.Join(t.TempDir(), "vault.db")
			db, legacy := newReleasedFixtureStore(t, dbPath, driver.driver, schemaV0170SQL, 48)
			n := createReleasedFixtureFile(t, legacy, "released.md", fakeHash("d4"), 8, "text/markdown")
			tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			var before bytes.Buffer
			require.NoError(t, exportReleasedMetadataSnapshot(ctx, tx, &before, 48))
			require.NoError(t, tx.Rollback())
			require.NoError(t, db.Close())
			s, err := Open(ctx, dbPath, driver.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			var after bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &after))
			require.Equal(t, before.String(), after.String(), "cutover preserves deterministic logical authority")
			require.FileExists(t, dbPath+".schema-v48.bak")
			require.NoError(t, s.ValidateMetadata(ctx))
			targets := noteTargets(t, s, notemarkdown.Recipe)
			require.Len(t, targets, 1)
			require.Equal(t, n.CurrentVersionID, targets[0].VersionID)
			publishNoteFixture(t, s, targets[0], "# Title\n", notemarkdown.Recipe)
			view, err := s.NoteByPath(ctx, "/released.md", notemarkdown.Recipe)
			require.NoError(t, err)
			require.Equal(t, "Title", view.Summary.Title)
		})
	}
}

func FuzzNoteCollectionSlashBoundary(f *testing.F) {
	f.Add("work", "note.md")
	f.Add("資料", "A:notes.md")
	f.Fuzz(func(t *testing.T, folder, name string) {
		if folder == "" || strings.ContainsAny(folder+name, "/\x00") {
			t.Skip()
		}
		watch := NoteWatch{"watch", "/" + folder}
		require.Equal(t, watch, NoteCollection("/"+folder+"/"+name, []NoteWatch{watch}))
		require.Equal(t, NoteWatch{Destination: "/"}, NoteCollection("/"+folder+"-sibling/"+name, []NoteWatch{watch}))
	})
}
