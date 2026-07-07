package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

func newTestIngester(t *testing.T) *Ingester {
	t.Helper()
	home := t.TempDir()
	blobsDir := filepath.Join(home, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	s, err := store.Open(filepath.Join(home, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return &Ingester{Store: s, Blobs: blob.New(blobsDir)}
}

// writeTree creates a synthetic source tree and returns its root.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	return root
}

func TestAddSingleFile(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	src := writeTree(t, map[string]string{"notes.txt": "hello"})

	rep, err := ing.AddPaths(ctx, []string{filepath.Join(src, "notes.txt")}, "/inbox")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Added)
	assert.Equal(t, 0, rep.Skipped)
	assert.Empty(t, rep.Failed)

	n, err := ing.Store.NodeByPath(ctx, "/inbox/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, "text/plain; charset=utf-8", n.MimeType)

	ok, err := ing.Blobs.Exists(n.BlobHash)
	require.NoError(t, err)
	assert.True(t, ok)

	// Source untouched.
	_, err = os.Stat(filepath.Join(src, "notes.txt"))
	require.NoError(t, err)
}

func TestAddDirectoryPreservesStructure(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	src := writeTree(t, map[string]string{
		"taxes/2024/return.txt": "2024 return",
		"taxes/2025/return.txt": "2025 return",
		"taxes/readme.txt":      "readme",
	})

	rep, err := ing.AddPaths(ctx, []string{filepath.Join(src, "taxes")}, "/archive")
	require.NoError(t, err)
	assert.Equal(t, 3, rep.Added)

	for _, p := range []string{
		"/archive/taxes/2024/return.txt",
		"/archive/taxes/2025/return.txt",
		"/archive/taxes/readme.txt",
	} {
		_, err := ing.Store.NodeByPath(ctx, p)
		assert.NoError(t, err, p)
	}
}

func TestAddRerunConverges(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	src := writeTree(t, map[string]string{
		"a.txt": "alpha",
		"b.txt": "beta",
	})

	rep1, err := ing.AddPaths(ctx,
		[]string{filepath.Join(src, "a.txt"), filepath.Join(src, "b.txt")}, "/inbox")
	require.NoError(t, err)
	assert.Equal(t, 2, rep1.Added)

	rep2, err := ing.AddPaths(ctx,
		[]string{filepath.Join(src, "a.txt"), filepath.Join(src, "b.txt")}, "/inbox")
	require.NoError(t, err)
	assert.Equal(t, 0, rep2.Added)
	assert.Equal(t, 2, rep2.Skipped)

	kids, err := ing.Store.Children(ctx, mustNode(t, ing, "/inbox").ID)
	require.NoError(t, err)
	assert.Len(t, kids, 2) // no duplicates
}

func mustNode(t *testing.T, ing *Ingester, path string) store.Node {
	t.Helper()
	n, err := ing.Store.NodeByPath(t.Context(), path)
	require.NoError(t, err)
	return n
}

func TestAddContinuesPastFailures(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	src := writeTree(t, map[string]string{"good.txt": "fine"})
	// A dangling symlink inside the tree must be reported, not fatal.
	require.NoError(t, os.Symlink(
		filepath.Join(src, "nope"), filepath.Join(src, "dangling")))

	rep, err := ing.AddPaths(ctx, []string{src}, "/inbox")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Added)
	require.Len(t, rep.Failed, 1)
	assert.Contains(t, rep.Failed[0].Path, "dangling")
}

// TestAddContinuesPastDirCollision covers a dir-creation failure mid-tree:
// a WalkDir callback error must not abort the whole AddPaths batch. Only
// the colliding subtree should be skipped; sibling sources still import.
func TestAddContinuesPastDirCollision(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()

	srcB := writeTree(t, map[string]string{"sub/b.txt": "beta"})
	srcA := writeTree(t, map[string]string{"a.txt": "alpha"})
	srcBBase := filepath.Base(srcB)

	// Pre-create a FILE node at the exact virtual path srcB's "sub"
	// subdirectory will later need ("/inbox/<srcBBase>/sub"), so that
	// MkdirAll for it collides with ErrNotDir during the real import.
	collision := writeTree(t, map[string]string{"sub": "not a directory"})
	_, err := ing.AddPaths(ctx, []string{filepath.Join(collision, "sub")}, "/inbox/"+srcBBase)
	require.NoError(t, err)

	rep, err := ing.AddPaths(ctx, []string{srcB, srcA}, "/inbox")
	require.NoError(t, err)

	require.NotEmpty(t, rep.Failed)
	found := false
	for _, f := range rep.Failed {
		if strings.Contains(f.Path, "sub") || strings.Contains(f.Err.Error(), "sub") {
			found = true
		}
	}
	assert.True(t, found, "expected a recorded failure for the colliding dir, got %+v", rep.Failed)

	assert.Equal(t, 1, rep.Added)
	_, err = ing.Store.NodeByPath(ctx, "/inbox/"+filepath.Base(srcA)+"/a.txt")
	require.NoError(t, err)
}

// A non-clean source argument ("docs/") must import like the clean spelling:
// WalkDir hands the root back as given while children are Join-cleaned, so
// without normalization the dirIDs lookup misses and the import aborts.
func TestAddDirectoryTrailingSlash(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	src := writeTree(t, map[string]string{"docs/notes.txt": "hello"})

	rep, err := ing.AddPaths(ctx,
		[]string{filepath.Join(src, "docs") + string(os.PathSeparator)}, "/inbox")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Added)
	assert.Empty(t, rep.Failed)

	_, err = ing.Store.NodeByPath(ctx, "/inbox/docs/notes.txt")
	require.NoError(t, err)
}
