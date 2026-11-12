package processing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/notemarkdown"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

func TestNoteOriginalBoundaries(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		src   []byte
		state string
	}{
		{"exactly4MiB", bytes.Repeat([]byte{'x'}, int(store.MaxNoteBytes)), "ok"},
		{"invalidUTF8", []byte{0xff}, "not_utf8"},
		{"headingLimit", []byte(strings.Repeat("# Heading\n", 4097)), "limit"},
		{"malformedYAML", []byte("---\ntitle: [broken\n---\n# Readable\n"), "ok"},
	} {
		t.Run(test.name, func(t *testing.T) {
			note, state, err := parseNoteOriginal(t.Context(), test.src)
			require.NoError(t, err)
			require.Equal(t, test.state, state)
			if state != "ok" {
				require.Equal(t, notemarkdown.Note{}, note)
			}
			if test.name == "malformedYAML" {
				require.Equal(t, "Readable", note.Title)
				require.NotNil(t, note.Frontmatter)
				require.Nil(t, note.Frontmatter.Raw)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := parseNoteOriginal(ctx, []byte("# Cancelled\n"))
	require.ErrorIs(t, err, context.Canceled)
}

func TestNoteBackfillDrainsBadNotes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		root := t.TempDir()
		catalog, err := store.Open(ctx, filepath.Join(root, "test.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, catalog.Close()) })
		require.NoError(t, os.MkdirAll(filepath.Join(root, "blobs", "tmp"), 0o700))
		blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, blobs.Close()) })
		for _, test := range []struct{ name, src string }{
			{"good.md", "---\ntags: [blue]\n---\n# Original\n"}, {"bad.md", "\xff"},
			{"malformed.md", "---\ntitle: [broken\n---\n# Readable\n"},
			{"indexed.md", "---\n? " + strings.Repeat("k", 2048) + "\n: x\n---\n# Readable\n"},
			{"expanded.md", "---\nbase: &m\n  ? " + strings.Repeat("a", 700000) + "\n  : 0\ncopies: [" + strings.Repeat("*m,", 39) + "*m]\n---\n# Readable\n"},
		} {
			require.NoError(t, blobs.WithMutation(ctx, func() error {
				hash, size, err := blobs.WriteContext(ctx, strings.NewReader(test.src))
				if err != nil {
					return err
				}
				_, err = catalog.CreateFile(ctx, catalog.RootID(), test.name, hash, size, "text/markdown")
				return err
			}))
		}
		// Oversize classification must not try to open unavailable blob bytes.
		_, err = catalog.CreateFile(ctx, catalog.RootID(), "oversize.md", strings.Repeat("a", 64), store.MaxNoteBytes+1, "text/markdown")
		require.NoError(t, err)
		page, err := catalog.ScanNoteTargetsAfter(ctx, notemarkdown.Recipe, "", 25)
		require.NoError(t, err)
		failure := errors.New("synthetic publication failure")
		failing := &notePublicationFailure{NoteCatalog: catalog, err: failure}
		for _, target := range page.Targets {
			if target.Path == "/good.md" {
				require.ErrorIs(t, processNoteTarget(ctx, failing, blobs, target), failure)
			}
		}
		require.Equal(t, 1, failing.calls, "ordinary errors retain retry behavior without terminal fallback")
		require.NoError(t, RebuildNotes(ctx, catalog, blobs))
		status, err := catalog.NotesStatus(ctx, notemarkdown.Recipe, nil)
		require.NoError(t, err)
		require.Equal(t, 2, status[0].Parsed)
		require.Equal(t, 4, status[0].Failed)
		require.Zero(t, status[0].Pending)
		view, err := catalog.NoteByPath(ctx, "/good.md", notemarkdown.Recipe)
		require.NoError(t, err)
		require.Equal(t, "Original", view.Summary.Title)
		require.Equal(t, []string{"blue"}, view.Summary.Tags)
		view, err = catalog.NoteByPath(ctx, "/malformed.md", notemarkdown.Recipe)
		require.NoError(t, err)
		require.Equal(t, "Readable", view.Summary.Title)
		view, err = catalog.NoteByPath(ctx, "/expanded.md", notemarkdown.Recipe)
		require.NoError(t, err)
		require.Equal(t, "limit", view.State, "Raw overflow completes as a terminal classification")
		require.Zero(t, view.HeadingCount)
		require.Nil(t, view.Frontmatter)
		view, err = catalog.NoteByPath(ctx, "/indexed.md", notemarkdown.Recipe)
		require.NoError(t, err)
		require.Equal(t, "limit", view.State)
		require.Zero(t, view.HeadingCount)
		require.Nil(t, view.Frontmatter)
		revision := catalog.WriteRevision()
		worker := NewNoteBackfill(catalog, blobs, nil, nil)
		workerPage, err := worker.ListPage(ctx, "", 25)
		require.NoError(t, err)
		require.Empty(t, workerPage.Targets)
		require.False(t, workerPage.EOF, "unchanged selected rows still advance")
		require.NotEmpty(t, workerPage.Next)
		workerPage, err = worker.ListPage(ctx, workerPage.Next, 25)
		require.NoError(t, err)
		require.True(t, workerPage.EOF)
		require.Equal(t, revision, catalog.WriteRevision())
	})
}

type notePublicationFailure struct {
	NoteCatalog
	err   error
	calls int
}

func (c *notePublicationFailure) PublishNoteParse(context.Context, store.NoteTarget, string, string, notemarkdown.Note) error {
	c.calls++
	return c.err
}

func TestNoteRebuildPropagatesCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	catalog := openDocumentEventTestStore(t)
	require.ErrorIs(t, RebuildNotes(ctx, catalog, nil), context.Canceled)
}

func TestNoteBackfillDrainsFiveThousandOriginals(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		root := t.TempDir()
		catalog, err := store.Open(ctx, filepath.Join(root, "test.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, catalog.Close()) })
		require.NoError(t, os.MkdirAll(filepath.Join(root, "blobs", "tmp"), 0o700))
		blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, blobs.Close()) })
		var hash string
		var size int64
		require.NoError(t, blobs.WithMutation(ctx, func() error {
			var err error
			hash, size, err = blobs.WriteContext(ctx, strings.NewReader("# Synthetic\n[Next](next.md)\n"))
			return err
		}))
		for i := range 5000 {
			_, err := catalog.CreateFile(ctx, catalog.RootID(), fmt.Sprintf("note-%04d.md", i), hash, size, "text/markdown")
			require.NoError(t, err)
		}
		worker := NewNoteBackfill(catalog, blobs, nil, nil)
		// Seed an entire unchanged linked page before later missing originals.
		page, err := worker.ListPage(ctx, "", worker.Page)
		require.NoError(t, err)
		require.Len(t, page.Targets, worker.Page)
		for _, target := range page.Targets {
			require.NoError(t, worker.Process(ctx, target))
		}
		worker.DrainOnce = true
		require.NoError(t, worker.Run(ctx))
		status, err := catalog.NotesStatus(ctx, notemarkdown.Recipe, nil)
		require.NoError(t, err)
		require.Equal(t, 5000, status[0].Parsed)
		require.Zero(t, status[0].Pending+status[0].Failed)
		for _, name := range []string{"note-0000.md", "note-2499.md", "note-4999.md"} {
			view, err := catalog.NoteByPath(ctx, "/"+name, notemarkdown.Recipe)
			require.NoError(t, err)
			require.Equal(t, "Synthetic", view.Summary.Title)
			require.Equal(t, 1, view.HeadingCount)
		}
		revision := catalog.WriteRevision()
		require.NoError(t, worker.Run(ctx), "all linked filtered pages reach EOF")
		require.Equal(t, revision, catalog.WriteRevision())
	})
}
