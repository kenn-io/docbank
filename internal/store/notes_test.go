package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
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
	_, cursor, err := s.VaultChanges(ctx, "", 500)
	require.NoError(t, err)
	publishNoteFixture(t, s, targets[0], src, notemarkdown.Recipe)
	changes, cursor, err := s.VaultChanges(ctx, cursor, 500)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.Equal(t, "processed", changes[0].Kind)
	require.Equal(t, n.ID, changes[0].NodeID)
	require.Equal(t, n.Revision, changes[0].Revision)
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
	changes, cursor, err = s.VaultChanges(ctx, cursor, 500)
	require.NoError(t, err)
	require.Empty(t, changes)
	require.Len(t, noteTargets(t, s, "next-recipe"), 1)
	// A duplicate heading fails after replacing the parse, rolling back all rows.
	invalid, err := notemarkdown.Parse(ctx, []byte(src), notemarkdown.Limits{})
	require.NoError(t, err)
	invalid.Headings = append(invalid.Headings, invalid.Headings[0])
	require.Error(t, s.PublishNoteParse(ctx, targets[0], "next-recipe", "ok", invalid))
	require.Equal(t, []int{1, 5, 1, 1, 1}, noteCatalogCounts(t, s, n.CurrentVersionID))
	var retainedRecipe string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT parsed_at,recipe FROM note_parses WHERE version_id=$1`, n.CurrentVersionID).Scan(&replayAt, &retainedRecipe))
	require.Equal(t, at, replayAt)
	require.Equal(t, notemarkdown.Recipe, retainedRecipe)
	changes, _, err = s.VaultChanges(ctx, cursor, 500)
	require.NoError(t, err)
	require.Empty(t, changes)
	publishNoteFixture(t, s, targets[0], src, "next-recipe")
	stale, err := s.NoteByPath(ctx, view.Summary.Path, notemarkdown.Recipe)
	require.NoError(t, err)
	require.Equal(t, "pending", stale.State)
	const replacement = "# Updated\n"
	_, version, err := s.ReplaceContent(ctx, n.ID, UnconditionalRev, fmt.Sprintf("%x", sha256.Sum256([]byte(replacement))), int64(len(replacement)), "text/markdown")
	require.NoError(t, err)
	// A late publication for the prior version must not recreate its rows.
	removed := pruneNoteFixtures(t, s)
	require.Equal(t, 1, removed)
	_, cursor, err = s.VaultChanges(ctx, "", 500)
	require.NoError(t, err)
	publishNoteFixture(t, s, targets[0], src, notemarkdown.Recipe)
	changes, cursor, err = s.VaultChanges(ctx, cursor, 500)
	require.NoError(t, err)
	require.Empty(t, changes)
	require.Equal(t, []int{0, 0, 0, 0, 0}, noteCatalogCounts(t, s, n.CurrentVersionID))
	targets = noteTargets(t, s, notemarkdown.Recipe)
	require.Len(t, targets, 1)
	require.Equal(t, version.ID, targets[0].VersionID)
	publishNoteFixture(t, s, targets[0], replacement, notemarkdown.Recipe)
	changes, _, err = s.VaultChanges(ctx, cursor, 500)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.Equal(t, "processed", changes[0].Kind)
	require.Equal(t, n.ID, changes[0].NodeID)
	require.Equal(t, targets[0].Revision, changes[0].Revision)
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
	t.Run("placement-limit", func(t *testing.T) {
		s := newStore(t)
		ctx := t.Context()
		longName := strings.Repeat("d", maxNoteIndexTextBytes)
		long, err := s.Mkdir(ctx, s.RootID(), longName)
		require.NoError(t, err)
		box, err := s.Mkdir(ctx, long.ID, "box")
		require.NoError(t, err)
		short, err := s.Mkdir(ctx, s.RootID(), "short")
		require.NoError(t, err)
		n := createNoteFixture(t, s, box.ID, "note.md", src, "text/markdown")
		note, err := notemarkdown.Parse(ctx, []byte(src), notemarkdown.Limits{})
		require.NoError(t, err)
		deepTarget := noteTargets(t, s, notemarkdown.Recipe)[0]
		require.ErrorIs(t, s.PublishNoteParse(ctx, deepTarget, notemarkdown.Recipe, "ok", note), ErrNoteIndexLimit)
		require.Equal(t, []int{0, 0, 0, 0, 0}, noteCatalogCounts(t, s, n.CurrentVersionID))
		require.NoError(t, s.PublishNoteParse(ctx, deepTarget, notemarkdown.Recipe, "limit", notemarkdown.Note{}))
		require.Equal(t, []int{1, 0, 0, 0, 0}, noteCatalogCounts(t, s, n.CurrentVersionID))
		view, err := s.NoteByPath(ctx, deepTarget.Path, notemarkdown.Recipe)
		require.NoError(t, err)
		require.Equal(t, "limit", view.State)
		require.Empty(t, noteTargets(t, s, notemarkdown.Recipe))
		_, _, err = s.Move(ctx, n.ID, box.ID, "note.md", UnconditionalRev)
		require.NoError(t, err)
		createNoteFixture(t, s, box.ID, "occupied.txt", "body", "text/plain")
		_, _, err = s.Move(ctx, n.ID, box.ID, "occupied.txt", UnconditionalRev)
		require.ErrorIs(t, err, ErrExists)
		_, _, err = s.Move(ctx, box.ID, long.ID, "box", UnconditionalRev)
		require.NoError(t, err)
		unrelated, err := s.Mkdir(ctx, s.RootID(), "unrelated")
		require.NoError(t, err)
		_, _, err = s.Move(ctx, unrelated.ID, s.RootID(), "unrelated-moved", UnconditionalRev)
		require.NoError(t, err)
		require.Empty(t, noteTargets(t, s, notemarkdown.Recipe), "unchanged limits stay settled")
		_, _, err = s.Move(ctx, n.ID, short.ID, "note.md", UnconditionalRev)
		require.NoError(t, err)
		targets := noteTargets(t, s, notemarkdown.Recipe)
		require.Len(t, targets, 1, "placement change must rediscover a limited current version")
		require.Equal(t, n.CurrentVersionID, targets[0].VersionID)
		require.Equal(t, "/short/note.md", targets[0].Path)
		require.NoError(t, s.PublishNoteParse(ctx, deepTarget, notemarkdown.Recipe, "limit", notemarkdown.Note{}))
		require.Equal(t, []int{0, 0, 0, 0, 0}, noteCatalogCounts(t, s, n.CurrentVersionID), "stale placement stays silent")
		publishNoteFixture(t, s, targets[0], src, notemarkdown.Recipe)
		var resolved string
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT target_path FROM note_links WHERE version_id=$1`, n.CurrentVersionID).Scan(&resolved))
		require.Equal(t, "/short/next.md", resolved)
		before, err := s.NodeByID(ctx, n.ID)
		require.NoError(t, err)
		_, _, err = s.Move(ctx, short.ID, long.ID, "moved", UnconditionalRev)
		require.NoError(t, err)
		targets = noteTargets(t, s, notemarkdown.Recipe)
		require.Len(t, targets, 1)
		require.Equal(t, before.CurrentVersionID, targets[0].VersionID)
		require.Equal(t, before.Revision, targets[0].Revision)
		require.ErrorIs(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "ok", note), ErrNoteIndexLimit)
		require.NoError(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "limit", notemarkdown.Note{}))
		require.Equal(t, []int{1, 0, 0, 0, 0}, noteCatalogCounts(t, s, n.CurrentVersionID))
		require.Empty(t, noteTargets(t, s, notemarkdown.Recipe))
		_, _, err = s.Move(ctx, short.ID, s.RootID(), "short", UnconditionalRev)
		require.NoError(t, err)
		targets = noteTargets(t, s, notemarkdown.Recipe)
		require.Len(t, targets, 1, "ancestor move must rediscover its limited descendant")
		require.Equal(t, before.Revision, targets[0].Revision)
		publishNoteFixture(t, s, targets[0], src, notemarkdown.Recipe)
		// Move deep once more, then replace its parent while preserving one
		// note's final path. Its limited sibling really moves shallow.
		_, _, err = s.Move(ctx, short.ID, long.ID, "moved", UnconditionalRev)
		require.NoError(t, err)
		targets = noteTargets(t, s, notemarkdown.Recipe)
		require.Len(t, targets, 1)
		require.ErrorIs(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "ok", note), ErrNoteIndexLimit)
		require.NoError(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "limit", notemarkdown.Note{}))
		sibling := createNoteFixture(t, s, short.ID, "sibling.md", src, "text/markdown")
		targets = noteTargets(t, s, notemarkdown.Recipe)
		require.Len(t, targets, 1)
		require.ErrorIs(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "ok", note), ErrNoteIndexLimit)
		require.NoError(t, s.PublishNoteParse(ctx, targets[0], notemarkdown.Recipe, "limit", notemarkdown.Note{}))
		_, err = s.Mkdir(ctx, s.RootID(), "replacement")
		require.NoError(t, err)
		deepPath := "/" + longName + "/moved"
		_, err = s.BatchMove(ctx, []BatchMoveRequest{
			{SourcePath: deepPath, DestinationPath: "/short"},
			{SourcePath: "/replacement", DestinationPath: deepPath},
			{SourcePath: deepPath + "/note.md", DestinationPath: deepPath + "/note.md"},
		})
		require.NoError(t, err)
		targets = noteTargets(t, s, notemarkdown.Recipe)
		require.Len(t, targets, 1, "only the sibling with a changed final path is retried")
		require.Equal(t, sibling.CurrentVersionID, targets[0].VersionID)
		require.Equal(t, "/short/sibling.md", targets[0].Path)
		require.Equal(t, []int{1, 0, 0, 0, 0}, noteCatalogCounts(t, s, n.CurrentVersionID))
		view, err = s.NoteByPath(ctx, deepPath+"/note.md", notemarkdown.Recipe)
		require.NoError(t, err)
		require.Equal(t, "limit", view.State, "unchanged final placement preserves the terminal result")
		publishNoteFixture(t, s, targets[0], src, notemarkdown.Recipe)
		_, err = s.BatchMove(ctx, []BatchMoveRequest{{SourcePath: deepPath + "/note.md", DestinationPath: deepPath + "/note.md"}})
		require.NoError(t, err)
		require.Empty(t, noteTargets(t, s, notemarkdown.Recipe))
		// Restore the limited descendant with its subtree before a worker can
		// prune it. Its separately trashed origin forces a shorter root path.
		parent, err := s.NodeByPath(ctx, deepPath)
		require.NoError(t, err)
		trashed, _, err := s.Trash(ctx, parent.ID, parent.Revision)
		require.NoError(t, err)
		_, _, err = s.Trash(ctx, long.ID, UnconditionalRev)
		require.NoError(t, err)
		_, restoredPath, err := s.Restore(ctx, trashed.ID, trashed.Revision)
		require.NoError(t, err)
		require.Equal(t, "/moved", restoredPath)
		targets = noteTargets(t, s, notemarkdown.Recipe)
		require.Len(t, targets, 1, "restored subtree must rediscover its limited descendant")
		require.Equal(t, n.ID, targets[0].NodeID)
		require.Equal(t, n.CurrentVersionID, targets[0].VersionID)
		require.Equal(t, "/moved/note.md", targets[0].Path)
		require.Equal(t, []int{0, 0, 0, 0, 0}, noteCatalogCounts(t, s, n.CurrentVersionID))
		publishNoteFixture(t, s, targets[0], src, notemarkdown.Recipe)
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT target_path FROM note_links WHERE version_id=$1`, n.CurrentVersionID).Scan(&resolved))
		require.Equal(t, "/moved/next.md", resolved)
		require.Empty(t, noteTargets(t, s, notemarkdown.Recipe))
	})
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
	defer func() { require.NoError(t, rows.Close()) }()
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
	t.Run("filename-fallback", func(t *testing.T) {
		for _, test := range []struct{ name, title string }{{".md", ".md"}, {"x.v2", "x"}} {
			t.Run(test.name, func(t *testing.T) {
				s := newStore(t)
				ctx := t.Context()
				createNoteFixture(t, s, s.RootID(), test.name, "body\n", "text/markdown")
				targets := noteTargets(t, s, notemarkdown.Recipe)
				require.Len(t, targets, 1)
				publishNoteFixture(t, s, targets[0], "body\n", notemarkdown.Recipe)
				view, err := s.NoteByPath(ctx, "/"+test.name, notemarkdown.Recipe)
				require.NoError(t, err)
				require.Equal(t, "ok", view.State)
				require.Equal(t, test.title, view.Summary.Title, "live filename stem for %q", test.name)
			})
		}
	})
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
			// Preserve an empty completion and a separate sealed, explicit request.
			at := time.Now().UTC().Add(time.Minute)
			faceAsset := albumAsset(t, legacy, "empty-scan.jpg")
			faceNode, err := legacy.NodeByID(ctx, faceAsset.Files[0].NodeID)
			require.NoError(t, err)
			faceRequest := photoVisionJobRequest(t, legacy, faceNode.CurrentVersionID, producerB, false, document.FaceDetectionProviderID)
			grantRenditionJobConsent(t, legacy, faceRequest)
			faceJob, faceWaiter, faceClaim := startPhotoDescription(t, legacy, faceRequest, at)
			publication, err := legacy.PublishPhotoDescription(ctx, faceClaim, faceDetectionResult(t), at)
			require.NoError(t, err)
			require.Equal(t, 1, publication.PublishedWaiterCount)
			completed, err := legacy.RenditionJobByID(ctx, faceJob.ID)
			require.NoError(t, err)
			require.Equal(t, RenditionJobCompleted, completed.State)
			published, err := legacy.RenditionJobWaiterByID(ctx, faceWaiter.ID)
			require.NoError(t, err)
			require.Equal(t, "published", published.State)
			explicitAsset := albumAsset(t, legacy, "explicit-request.jpg")
			explicitNode, err := legacy.NodeByID(ctx, explicitAsset.Files[0].NodeID)
			require.NoError(t, err)
			request := photoDescriptionJobRequest(t, legacy, explicitNode.CurrentVersionID, producerB, true)
			grantRenditionJobConsent(t, legacy, request)
			job, waiter, claim := startPhotoDescription(t, legacy, request, at)
			require.NoError(t, legacy.CheckpointRenditionProvider(ctx, claim, "synthetic-upgrade-handle", at.Add(time.Second)))
			work, err := legacy.RenditionJobWorkByClaim(ctx, claim, at.Add(time.Second))
			require.NoError(t, err)
			require.Equal(t, RenditionJobRunning, work.Job.State)
			require.Equal(t, RenditionPhaseProvider, work.Job.Phase)
			require.Equal(t, 1, work.Job.ProviderAttempts)
			require.EqualValues(t, 1, claim.Epoch)
			require.Equal(t, waiter.ID, work.Waiter.ID)
			require.NotNil(t, work.ExecutionSnapshot)
			// Match released Open's lexical startup before freezing its snapshot.
			_, err = legacy.MigrateLegacyPlainText(ctx)
			require.NoError(t, err)
			tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			var before bytes.Buffer
			require.NoError(t, exportReleasedMetadataSnapshot(ctx, tx, &before, 48))
			tables := []string{"photo_face_scans", "rendition_jobs", "rendition_job_waiters", "photo_description_explicit_waiters"}
			operational := make(map[string][]string)
			for i, table := range tables {
				var rows strings.Builder
				dumpReleasedTable(t, tx, table, &rows)
				operational[table] = strings.Split(strings.TrimSpace(rows.String()), "\n")
				slices.Sort(operational[table])
				require.Len(t, operational[table], []int{1, 2, 2, 1}[i], table)
			}
			require.Contains(t, operational["photo_face_scans"][0], faceNode.CurrentVersionID)
			for _, id := range []string{faceJob.ID, job.ID, faceWaiter.ID, waiter.ID} {
				require.NotEmpty(t, id)
				require.Contains(t, strings.Join(append(slices.Clone(operational["rendition_jobs"]), operational["rendition_job_waiters"]...), "\n"), id)
			}
			require.Contains(t, operational["photo_description_explicit_waiters"][0], waiter.ID)
			require.NoError(t, tx.Rollback())
			require.NoError(t, db.Close())
			s, err := Open(ctx, dbPath, driver.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			var after bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &after))
			require.Equal(t, before.String(), after.String(), "cutover preserves deterministic logical authority")
			for _, table := range tables {
				var rows strings.Builder
				dumpReleasedTable(t, s.db, table, &rows)
				restored := strings.Split(strings.TrimSpace(rows.String()), "\n")
				slices.Sort(restored)
				require.Equal(t, operational[table], restored, "cutover preserves complete operational rows: %s", table)
			}
			var faces int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM photo_faces WHERE content_version_id=$1`, faceNode.CurrentVersionID).Scan(&faces))
			require.Zero(t, faces, "the scan completion represents a genuine empty result")
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
			// Only after exact preservation checks, exercise the retained lease/fence.
			_, err = s.ClaimRenditionJob(ctx, job.ID, "upgrade-resume", at.Add(30*time.Minute), time.Hour)
			require.ErrorIs(t, err, ErrRenditionJobLeaseHeld)
			resumeAt := at.Add(2 * time.Hour)
			resumed, err := s.ClaimRenditionJob(ctx, job.ID, "upgrade-resume", resumeAt, time.Hour)
			require.NoError(t, err)
			require.Equal(t, claim.Epoch+1, resumed.Epoch)
			require.Equal(t, work.Job.ProviderAttempts, resumed.ProviderAttempts)
			require.Equal(t, "synthetic-upgrade-handle", resumed.ResumeHandle)
			require.ErrorIs(t, s.CheckpointRenditionProvider(ctx, claim, "stale-handle", resumeAt), ErrRenditionJobFenced)
			resumedWork, err := s.RenditionJobWorkByClaim(ctx, resumed, resumeAt)
			require.NoError(t, err)
			require.Equal(t, work.ExecutionSnapshot, resumedWork.ExecutionSnapshot)
			require.Equal(t, work.Waiter, resumedWork.Waiter)
			authorization, err := s.BeginRenditionProvider(ctx, resumed, waiter.ID, resumeAt)
			require.NoError(t, err, "resume uses the stored snapshot without starting another attempt")
			require.Equal(t, work.Waiter.AuthorizationGrantID, authorization.GrantID)
			require.Equal(t, work.Waiter.AuthorizationIncarnationID, authorization.ProcessingIncarnationID)
			require.Equal(t, work.Waiter.AuthorizationRevocationFence, authorization.RevocationFence)
			resumedWork, err = s.RenditionJobWorkByClaim(ctx, resumed, resumeAt)
			require.NoError(t, err)
			require.Equal(t, work.Job.ProviderAttempts, resumedWork.Job.ProviderAttempts)
			require.Equal(t, work.ExecutionSnapshot, resumedWork.ExecutionSnapshot)
			require.Equal(t, work.Waiter, resumedWork.Waiter)
			var handle string
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT provider_resume_handle FROM rendition_jobs WHERE job_id=$1`, job.ID).Scan(&handle))
			require.Equal(t, resumed.ResumeHandle, handle)
			require.NoError(t, s.ValidateMetadata(ctx))
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
