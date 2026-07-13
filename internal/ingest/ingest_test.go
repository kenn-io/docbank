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
	blobs, err := blob.New(store.NewPackCatalog(s), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blobs.Close() })
	return &Ingester{Store: s, Blobs: blobs}
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

func TestAddDotDotSourceStaysUnderDest(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	src := writeTree(t, map[string]string{"sub/a.txt": "hello"})
	t.Chdir(filepath.Join(src, "sub"))

	// A source spelled ".." must import under its real basename, not climb
	// out of the destination via Join(dest, "..", rel).
	rep, err := ing.AddPaths(ctx, []string{".."}, "/inbox")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Added)
	assert.Empty(t, rep.Failed)

	top := filepath.Base(src)
	_, err = ing.Store.NodeByPath(ctx, "/inbox/"+top+"/sub/a.txt")
	require.NoError(t, err)

	// Nothing escaped to the tree root: it holds exactly the dest dir.
	kids, err := ing.Store.Children(ctx, ing.Store.RootID())
	require.NoError(t, err)
	require.Len(t, kids, 1)
	assert.Equal(t, "inbox", kids[0].Name)
}

func TestAddDotSourceUsesRealBasename(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	src := writeTree(t, map[string]string{"a.txt": "hello"})
	t.Chdir(src)

	rep, err := ing.AddPaths(ctx, []string{"."}, "/inbox")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Added)

	_, err = ing.Store.NodeByPath(ctx, "/inbox/"+filepath.Base(src)+"/a.txt")
	require.NoError(t, err)
}

func TestAddTreeStaleDestinationIsNotResurrected(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	src := writeTree(t, map[string]string{"sub/a.txt": "x"})

	dest, err := ing.Store.MkdirAll(ctx, "/inbox")
	require.NoError(t, err)
	// The destination is trashed after the command resolved its id —
	// the concurrent-trash shape. Import must fail per-directory, not
	// re-create a live /inbox from the stale path and fill it.
	_, _, err = ing.Store.Trash(ctx, dest.ID, store.UnconditionalRev)
	require.NoError(t, err)

	ingestID, err := ing.Store.BeginIngest(ctx, "cli", "test")
	require.NoError(t, err)
	var rep Report
	require.NoError(t, ing.addTree(ctx, &rep, ingestID, dest.ID, src, src, exclusions{}))
	assert.NotEmpty(t, rep.Failed)
	assert.Zero(t, rep.Added)

	_, err = ing.Store.NodeByPath(ctx, "/inbox")
	assert.ErrorIs(t, err, store.ErrNotFound, "trashed destination must stay trashed")
}

func TestPreflightInventoriesWithoutMutatingVault(t *testing.T) {
	ing := newTestIngester(t)
	src := writeTree(t, map[string]string{
		"keep/report.PDF":        "report",
		"keep/readme":            "read me",
		".git/config":            "secret-ish metadata",
		"project/cache/data.bin": "cache",
		"project/data.jsonl":     "{\"ok\":true}\n",
	})

	report, err := Preflight(t.Context(), []string{src}, Options{
		Exclude: []string{".git", "project/cache"},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(3), report.Files)
	assert.Equal(t, int64(len("report")+len("read me")+len("{\"ok\":true}\n")), report.LogicalBytes)
	assert.Equal(t, int64(3), report.PackEligible.Files)
	assert.Zero(t, report.LooseOnly.Files)
	assert.Zero(t, report.Rejected.Files)
	assert.Equal(t, int64(2), report.Excluded)
	assert.Zero(t, report.Errors)
	require.Len(t, report.FileTypes, 3)
	assert.Equal(t, ".jsonl", report.FileTypes[0].Extension)
	assert.Empty(t, report.FileTypes[1].Extension)
	assert.Equal(t, ".pdf", report.FileTypes[2].Extension)

	_, err = ing.Store.NodeByPath(t.Context(), "/inbox")
	require.ErrorIs(t, err, store.ErrNotFound, "preflight must not create a destination or ingest rows")
}

func TestPreflightSizePolicyBoundaries(t *testing.T) {
	var report PreflightReport
	types := make(map[string]FileType)
	report.addFile("packed.bin", blob.MaxPackedBlobBytes, types)
	report.addFile("loose.bin", blob.MaxPackedBlobBytes+1, types)
	report.addFile("limit.bin", blob.MaxIngestBytes, types)
	report.addFile("rejected.bin", blob.MaxIngestBytes+1, types)

	assert.Equal(t, int64(1), report.PackEligible.Files)
	assert.Equal(t, int64(2), report.LooseOnly.Files)
	assert.Equal(t, int64(1), report.Rejected.Files)
}

func TestAddPathsHonorsPreflightExclusions(t *testing.T) {
	ing := newTestIngester(t)
	src := writeTree(t, map[string]string{
		"keep.txt":               "keep",
		".git/config":            "exclude",
		"project/cache/data.bin": "exclude",
		"project/session.jsonl":  "keep",
	})

	report, err := ing.AddPathsWithOptions(t.Context(), []string{src}, "/inbox", Options{
		Exclude: []string{".git", "project/cache"},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, report.Added)
	assert.Equal(t, 2, report.Excluded)
	assert.Empty(t, report.Failed)

	top := "/inbox/" + filepath.Base(src)
	_, err = ing.Store.NodeByPath(t.Context(), top+"/keep.txt")
	require.NoError(t, err)
	_, err = ing.Store.NodeByPath(t.Context(), top+"/project/session.jsonl")
	require.NoError(t, err)
	_, err = ing.Store.NodeByPath(t.Context(), top+"/.git")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = ing.Store.NodeByPath(t.Context(), top+"/project/cache")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestPreflightRejectsUnsafeExclusionRules(t *testing.T) {
	for _, rule := range []string{
		"", ".", "..", "../outside", "inside/../other", string(filepath.Separator) + "absolute",
	} {
		t.Run(rule, func(t *testing.T) {
			_, err := Preflight(t.Context(), nil, Options{Exclude: []string{rule}})
			require.Error(t, err)
		})
	}
}

func TestAddMissingSourceIsReportedNotFatal(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	src := writeTree(t, map[string]string{"good.txt": "hello"})

	// A missing top-level source is a per-file failure like any other:
	// the good file's import must complete and be reported, not vanish
	// behind an early error return.
	rep, err := ing.AddPaths(ctx,
		[]string{filepath.Join(src, "good.txt"), filepath.Join(src, "missing.txt")}, "/inbox")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Added)
	require.Len(t, rep.Failed, 1)
	assert.Equal(t, filepath.Join(src, "missing.txt"), rep.Failed[0].Path)

	_, err = ing.Store.NodeByPath(ctx, "/inbox/good.txt")
	require.NoError(t, err)
}
