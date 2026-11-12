package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/jsontext"
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
	var targets []NoteTarget
	for cursor := ""; ; {
		page, err := s.ScanNoteTargetsAfter(t.Context(), recipe, cursor, 100)
		require.NoError(t, err)
		for _, target := range page.Targets {
			if !target.Prune {
				targets = append(targets, target)
			}
		}
		if page.EOF {
			return targets
		}
		require.Greater(t, page.Next, cursor)
		cursor = page.Next
	}
}

func pruneNoteFixtures(t *testing.T, s *Store) int {
	t.Helper()
	removed := 0
	for cursor := ""; ; {
		page, err := s.ScanNoteTargetsAfter(t.Context(), notemarkdown.Recipe, cursor, 100)
		require.NoError(t, err)
		for _, target := range page.Targets {
			if target.Prune {
				require.NoError(t, s.PruneNoteParse(t.Context(), target))
				removed++
			}
		}
		if page.EOF {
			return removed
		}
		cursor = page.Next
	}
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
	removed := pruneNoteFixtures(t, s)
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
	pruneNoteFixtures(t, s)
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
	pruneNoteFixtures(t, s)
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
	targets := noteTargets(t, s, notemarkdown.Recipe)
	require.Len(t, targets, 3)
	require.Error(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "limit", notemarkdown.Note{Title: "partial"}))
	for i, target := range targets {
		state := []string{"not_utf8", "limit", "ok"}[i]
		note := notemarkdown.Note{}
		if state == "ok" {
			note.Lines = 1
		}
		require.NoError(t, s.PublishNoteParse(ctx, target, notemarkdown.Recipe, state, note))
	}
	oversize, err := s.CreateFile(ctx, s.RootID(), "large.md", fakeHash("b2"), MaxNoteBytes+1, "text/plain")
	require.NoError(t, err)
	targets = noteTargets(t, s, notemarkdown.Recipe)
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
	// B admits this maximal key; its index tuple also includes the actual
	// 36-byte version and one-byte scalar value.
	src := "---\n? " + strings.Repeat("k", 2048-36-1) + "\n: x\n---\n# Readable\n"
	n := createNoteFixture(t, s, s.RootID(), "indexed.md", src, "text/markdown")
	note, err := notemarkdown.Parse(ctx, []byte(src), notemarkdown.Limits{})
	require.NoError(t, err)
	var target NoteTarget
	for _, candidate := range noteTargets(t, s, notemarkdown.Recipe) {
		if candidate.VersionID == n.CurrentVersionID {
			target = candidate
		}
	}
	require.NoError(t, s.PublishNoteParse(ctx, target, notemarkdown.Recipe, "ok", note))
	require.Equal(t, []int{1, 1, 1, 0, 0}, noteCatalogCounts(t, s, n.CurrentVersionID))
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
		page, err := s.ScanNoteTargetsAfter(ctx, notemarkdown.Recipe, cursor, 100)
		require.NoError(t, err)
		if page.EOF {
			break
		}
		for _, target := range page.Targets {
			publishNoteFixture(t, s, target, src, notemarkdown.Recipe)
			published++
		}
		cursor = page.Next
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
	for _, test := range []struct {
		path    string
		watches []NoteWatch
		want    int
	}{
		{"/work/deep/n.md", watches, 1},
		{"/work/n.md", watches, 0},
		{"/workshop/n.md", watches, 2},
		{"/n.md", nil, 0},
	} {
		destinations, err := noteDestinations(test.watches)
		require.NoError(t, err)
		got := noteCollection(test.path, test.watches, destinations)
		require.Equal(t, test.want, got)
	}
}

func checkNoteAuthoredValuesAndCollections(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	dir, err := s.Mkdir(ctx, s.RootID(), "work")
	require.NoError(t, err)
	deep, err := s.Mkdir(ctx, dir.ID, "café")
	require.NoError(t, err)
	const numbers = "---\nlarge: 9007199254740993\nsigned: -9223372036854775808\nunsigned: 18446744073709551615\nnested: {values: [9007199254740993, 18446744073709551615]}\n---\n# Readable\n"
	for i, test := range []struct {
		src, diagnostic string
	}{
		{numbers, ""},
		{"---\ntitle: [broken\n---\n# Readable\n", "invalid_frontmatter"},
		{"---\n# Readable\n", "unclosed_frontmatter"},
		{"# Readable\n", ""},
		{"---\n---\n# Readable\n", ""},
	} {
		name := fmt.Sprintf("note-%d.md", i)
		n := createNoteFixture(t, s, deep.ID, name, test.src, "text/markdown")
		targets := noteTargets(t, s, notemarkdown.Recipe)
		require.Len(t, targets, 1)
		publishNoteFixture(t, s, targets[0], test.src, notemarkdown.Recipe)
		view, err := s.NoteByPath(ctx, "/work/café/"+name, notemarkdown.Recipe)
		require.NoError(t, err)
		require.Equal(t, "ok", view.State)
		require.Equal(t, "Readable", view.Summary.Title)
		require.Equal(t, test.diagnostic, view.FrontmatterError)
		var stored *string
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT frontmatter_error FROM note_parses WHERE version_id=$1`, n.CurrentVersionID).Scan(&stored))
		if test.diagnostic != "" {
			require.Equal(t, &test.diagnostic, stored)
			require.Nil(t, view.Frontmatter)
		} else {
			require.Nil(t, stored)
		}
		if i == 0 {
			require.Equal(t, jsontext.Value(`9007199254740993`), view.Frontmatter["large"])
			require.Equal(t, jsontext.Value(`-9223372036854775808`), view.Frontmatter["signed"])
			require.Equal(t, jsontext.Value(`18446744073709551615`), view.Frontmatter["unsigned"])
			require.Equal(t, jsontext.Value(`{"values":[9007199254740993,18446744073709551615]}`), view.Frontmatter["nested"])
		}
		if i == 3 {
			require.Nil(t, view.Frontmatter)
		}
		if i == 4 {
			require.NotNil(t, view.Frontmatter)
			require.Empty(t, view.Frontmatter)
		}
	}
	const identities = "---\naliases: [Cafe\u0301, Café, '..odd:alias']\n---\n[Relative](cafe%CC%81.md)\n[Absolute](/work/cafe%CC%81/other.md)\n[[Cafe\u0301]]\n"
	n := createNoteFixture(t, s, deep.ID, "identities.md", identities, "text/markdown")
	publishNoteFixture(t, s, noteTargets(t, s, notemarkdown.Recipe)[0], identities, notemarkdown.Recipe)
	rows, err := s.db.QueryContext(ctx, `SELECT target_path,target_key FROM note_links WHERE version_id=$1 ORDER BY ordinal`, n.CurrentVersionID)
	require.NoError(t, err)
	var paths, keys []*string
	for rows.Next() {
		var targetPath, targetKey *string
		require.NoError(t, rows.Scan(&targetPath, &targetKey))
		paths, keys = append(paths, targetPath), append(keys, targetKey)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, []*string{new("/work/café/café.md"), new("/work/café/other.md"), nil}, paths)
	require.Equal(t, []*string{nil, nil, new("café")}, keys)
	var alias, key string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT alias,alias_key FROM note_aliases WHERE version_id=$1 AND alias_key=$2`, n.CurrentVersionID, "café").Scan(&alias, &key))
	require.Equal(t, "Cafe\u0301", alias, "first authored spelling remains display data")
	require.Equal(t, "café", key)
	var aliasCount int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM note_aliases WHERE version_id=$1`, n.CurrentVersionID).Scan(&aliasCount))
	require.Equal(t, 2, aliasCount, "NFC aliases deduplicate while arbitrary punctuation remains valid")
	createNoteFixture(t, s, dir.ID, "outer.md", "body", "text/markdown")
	createNoteFixture(t, s, s.RootID(), "root.md", "body", "text/markdown")
	watches := []NoteWatch{{"outer", "/work/"}, {"inner", "//work//cafe\u0301///"}, {"root", "//"}}
	status, err := s.NotesStatus(ctx, notemarkdown.Recipe, watches)
	require.NoError(t, err)
	require.Equal(t, 1, status[0].Pending)
	require.Equal(t, 6, status[1].Parsed)
	require.Equal(t, 1, status[2].Pending)
	require.Zero(t, status[3].Parsed+status[3].Pending+status[3].Failed)
}

func checkNoteBoundedPagesAndPrune(t *testing.T, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	const src = "# Linked\n[Next](next.md)\n"
	for i := range 5 {
		dir, err := s.Mkdir(ctx, s.RootID(), fmt.Sprintf("dir-%d", i))
		require.NoError(t, err)
		createNoteFixture(t, s, dir.ID, "note.md", src, "text/markdown")
	}
	targets := noteTargets(t, s, notemarkdown.Recipe)
	require.Len(t, targets, 5)
	for _, target := range targets[:4] {
		publishNoteFixture(t, s, target, src, notemarkdown.Recipe)
	}
	var parents []int64
	for _, target := range targets[2:4] {
		n, err := s.NodeByID(ctx, target.NodeID)
		require.NoError(t, err)
		parents = append(parents, *n.ParentID)
		_, _, err = s.Trash(ctx, *n.ParentID, UnconditionalRev)
		require.NoError(t, err)
	}
	cursor := ""
	var prunes []NoteTarget
	for i := range 5 {
		page, err := s.ScanNoteTargetsAfter(ctx, notemarkdown.Recipe, cursor, 1)
		require.NoError(t, err)
		require.False(t, page.EOF)
		require.Equal(t, targets[i].VersionID, page.Next)
		if i < 2 {
			require.Empty(t, page.Targets, "unchanged linked rows consume one bounded page")
		} else {
			require.Len(t, page.Targets, 1)
			if i < 4 {
				require.True(t, page.Targets[0].Prune, "trashed ancestry cannot be derived")
				prunes = append(prunes, page.Targets[0])
			} else {
				require.False(t, page.Targets[0].Prune)
				publishNoteFixture(t, s, page.Targets[0], src, notemarkdown.Recipe)
			}
		}
		cursor = page.Next
	}
	page, err := s.ScanNoteTargetsAfter(ctx, notemarkdown.Recipe, cursor, 1)
	require.NoError(t, err)
	require.True(t, page.EOF)
	require.Empty(t, page.Next)
	_, _, err = s.Restore(ctx, parents[0], UnconditionalRev)
	require.NoError(t, err)
	require.NoError(t, s.PruneNoteParse(ctx, prunes[0]))
	require.Equal(t, []int{1, 0, 1, 1, 0}, noteCatalogCounts(t, s, prunes[0].VersionID), "stale cleanup cannot delete restored ownership")
	require.NoError(t, s.PruneNoteParse(ctx, prunes[1]))
	// A stale derivation after ancestor trash must not recreate pruned rows.
	publishNoteFixture(t, s, targets[3], src, notemarkdown.Recipe)
	require.Equal(t, []int{0, 0, 0, 0, 0}, noteCatalogCounts(t, s, prunes[1].VersionID))
	require.Empty(t, noteTargets(t, s, notemarkdown.Recipe))
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

func TestNoteMetadataRestoreLeavesNotesPending(t *testing.T) {
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
}

func TestUpgradeReleasedV0170RebuildsNoteCatalog(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			ctx := t.Context()
			dbPath := filepath.Join(t.TempDir(), "vault.db")
			db, legacy := newReleasedFixtureStore(t, dbPath, driver.driver, schemaV0170SQL, 48)
			run, err := legacy.BeginIngest(ctx, "watch", "fixture")
			require.NoError(t, err)
			n, err := legacy.IngestFileExact(ctx, run, legacy.RootID(), "released.md", fakeHash("d4"), 8, "text/markdown", "released.md", "")
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `INSERT INTO watch_source_absences(watch_name,source_ref,absent_since) VALUES('fixture','released.md','2026-01-01T00:00:00.000000000Z')`)
			require.NoError(t, err)
			// Match released Open's lexical startup before freezing its snapshot.
			_, err = legacy.MigrateLegacyPlainText(ctx)
			require.NoError(t, err)
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
			pending, err := s.NoteByPath(ctx, "/released.md", notemarkdown.Recipe)
			require.NoError(t, err)
			require.Equal(t, "gone", pending.Summary.SourceState)
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

func TestNoteIndexedTupleAdmission(t *testing.T) {
	t.Parallel()
	target := NoteTarget{VersionID: strings.Repeat("v", 36), Path: "/note.md"}
	canonicalTarget := "/" + strings.Repeat("e\u0301/", 682) + "x"
	for _, test := range []struct {
		name string
		note notemarkdown.Note
	}{
		{"field", notemarkdown.Note{Frontmatter: &notemarkdown.Frontmatter{Fields: []notemarkdown.Field{{Key: strings.Repeat("é", 1005), Value: "xx"}}}}},
		{"alias", notemarkdown.Note{Frontmatter: &notemarkdown.Frontmatter{Aliases: []string{strings.Repeat("E\u0301", 1006)}}}},
		{"path", notemarkdown.Note{Links: []notemarkdown.Link{{Form: "markdown", Target: canonicalTarget}}}},
		{"wiki", notemarkdown.Note{Links: []notemarkdown.Link{{Form: "wikilink", Target: strings.Repeat("E\u0301", 1024)}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, validateNoteIndexes(t.Context(), target, test.note))
			if f := test.note.Frontmatter; f != nil {
				if len(f.Fields) > 0 {
					f.Fields[0].Value += "x"
				} else {
					f.Aliases[0] += "x"
				}
			} else {
				test.note.Links[0].Target += "x"
			}
			require.ErrorIs(t, validateNoteIndexes(t.Context(), target, test.note), ErrNoteIndexLimit)
		})
	}
	var fields []notemarkdown.Field
	for i := range 64 {
		fields = append(fields, notemarkdown.Field{Key: fmt.Sprintf("key-%d", i), Value: strings.Repeat("x", 1024)})
	}
	require.NoError(t, validateNoteIndexes(t.Context(), target, notemarkdown.Note{Frontmatter: &notemarkdown.Frontmatter{Fields: fields}}), "each tuple fits despite total exceeding 2048 bytes")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, validateNoteIndexes(ctx, target, notemarkdown.Note{Frontmatter: &notemarkdown.Frontmatter{Fields: fields}}), context.Canceled)
}
