package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

func newTestIngester(t *testing.T) *Ingester {
	t.Helper()
	return newTestIngesterWithOptions(t, blob.Options{})
}

func newTestIngesterWithOptions(t *testing.T, options blob.Options) *Ingester {
	t.Helper()
	home := t.TempDir()
	blobsDir := filepath.Join(home, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	s, err := store.Open(filepath.Join(home, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	blobs, err := blob.NewWithOptions(store.NewPackCatalog(s), blobsDir, options)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blobs.Close() })
	return &Ingester{Store: s, Blobs: blobs}
}

func TestAddRecordsCompressedLooseAuthority(t *testing.T) {
	ing := newTestIngesterWithOptions(t, blob.ManagedOptions())
	content := strings.Repeat("compressible local document\n", 512)
	src := writeTree(t, map[string]string{"document.txt": content})

	report, err := ing.AddPaths(
		t.Context(), []string{filepath.Join(src, "document.txt")}, "/inbox",
	)
	require.NoError(t, err)
	require.Equal(t, 1, report.Added)
	node, err := ing.Store.NodeByPath(t.Context(), "/inbox/document.txt")
	require.NoError(t, err)
	physical, err := ing.Store.PhysicalContent(t.Context(), node.BlobHash)
	require.NoError(t, err)
	assert.Equal(t, "loose", physical.Kind)
	assert.Equal(t, "zstd", physical.Encoding)
	assert.Equal(t, int64(len(content)), physical.LogicalBytes)
	assert.Less(t, physical.StoredBytes, physical.LogicalBytes)
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

func TestAddPathsRejectsNonUTF8ExplicitSourceWithoutPoisoningMetadata(t *testing.T) {
	ing := newTestIngester(t)
	invalid := filepath.Join(t.TempDir(), "invalid-"+string([]byte{0xff})+".txt")
	require.False(t, utf8.ValidString(invalid))

	preflight, err := Preflight(t.Context(), []string{invalid}, Options{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), preflight.Errors)
	require.Len(t, preflight.Findings, 1)
	assert.Equal(t, strconv.QuoteToASCII(invalid), preflight.Findings[0].Path)

	rep, err := ing.AddPaths(t.Context(), []string{invalid}, "/inbox")
	require.NoError(t, err)
	assert.Zero(t, rep.Added)
	require.Len(t, rep.Failed, 1)
	assert.Equal(t, strconv.QuoteToASCII(invalid), rep.Failed[0].Path)
	require.ErrorContains(t, rep.Failed[0].Err, "is not valid UTF-8")
	var metadata bytes.Buffer
	require.NoError(t, ing.Store.ExportMetadata(t.Context(), &metadata))
	assert.True(t, utf8.Valid(metadata.Bytes()))
}

func TestAddProgressReportsBytesAndFinalOutcome(t *testing.T) {
	ing := newTestIngester(t)
	src := writeTree(t, map[string]string{"a.txt": "alpha", "b.txt": "beta"})
	paths := []string{filepath.Join(src, "a.txt"), filepath.Join(src, "b.txt")}
	var events []ProgressEvent

	rep, err := ing.AddPathsWithOptions(t.Context(), paths, "/inbox", Options{
		Progress: func(event ProgressEvent) { events = append(events, event) },
	})
	require.NoError(t, err)
	assert.Equal(t, 2, rep.Added)
	require.NotEmpty(t, events)
	assert.Zero(t, events[0].FilesDone)
	assert.False(t, events[0].Final)
	last := events[len(events)-1]
	assert.True(t, last.Final)
	assert.Equal(t, int64(2), last.FilesDone)
	assert.Equal(t, int64(len("alpha")+len("beta")), last.BytesRead)
	assert.Equal(t, 2, last.Added)
	assert.Zero(t, last.Failed)
}

func TestAddCancellationDoesNotAuthorizeIncompleteFile(t *testing.T) {
	ing := newTestIngester(t)
	src := filepath.Join(t.TempDir(), "large.bin")
	f, err := os.Create(src)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(progressByteInterval*2))
	require.NoError(t, f.Close())

	ctx, cancel := context.WithCancel(t.Context())
	var events []ProgressEvent
	_, err = ing.AddPathsWithOptions(ctx, []string{src}, "/inbox", Options{
		Progress: func(event ProgressEvent) {
			events = append(events, event)
			if event.BytesRead >= progressByteInterval {
				cancel()
			}
		},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled, err)
	require.NotEmpty(t, events)
	assert.False(t, events[len(events)-1].Final)
	_, err = ing.Store.NodeByPath(t.Context(), "/inbox/large.bin")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"cancellation during blob reading must not grant node authority")
}

type cancelingContentReader struct {
	cancel context.CancelFunc
	sent   bool
}

func (r *cancelingContentReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.sent {
		return 0, context.Canceled
	}
	r.sent = true
	p[0] = 'x'
	r.cancel()
	return 1, nil
}

func TestReplaceContentCancellationLeavesCurrentAuthorityUntouched(t *testing.T) {
	ing := newTestIngester(t)
	var created store.Node
	require.NoError(t, ing.Blobs.WithMutation(t.Context(), func() error {
		hash, size, err := ing.Blobs.WriteContext(t.Context(), strings.NewReader("current"))
		if err != nil {
			return err
		}
		created, err = ing.Store.CreateFile(
			t.Context(), ing.Store.RootID(), "current.txt", hash, size, "text/plain",
		)
		return err
	}))

	ctx, cancel := context.WithCancel(t.Context())
	_, err := ing.ReplaceContent(ctx, created.ID, created.Revision, "text/plain",
		&cancelingContentReader{cancel: cancel}, strings.Repeat("0", 64), 2)
	require.ErrorIs(t, err, context.Canceled)
	unchanged, err := ing.Store.NodeByID(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.Revision, unchanged.Revision)
	assert.Equal(t, created.CurrentVersionID, unchanged.CurrentVersionID)
	versions, total, err := ing.Store.ContentVersions(t.Context(), created.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, versions, 1)
}

func TestAddProgressBatchesManySmallFiles(t *testing.T) {
	ing := newTestIngester(t)
	files := make(map[string]string, 130)
	for i := range 130 {
		files[fmt.Sprintf("%03d.txt", i)] = "x"
	}
	src := writeTree(t, files)
	var events []ProgressEvent

	rep, err := ing.AddPathsWithOptions(t.Context(), []string{src}, "/inbox", Options{
		Progress: func(event ProgressEvent) { events = append(events, event) },
	})
	require.NoError(t, err)
	assert.Equal(t, 130, rep.Added)
	require.GreaterOrEqual(t, len(events), 4, "initial, two batches, and final")
	assert.Less(t, len(events), rep.Added, "progress must not emit one event per small file")
	assert.True(t, events[len(events)-1].Final)
	assert.Equal(t, int64(130), events[len(events)-1].FilesDone)
}

func TestAddCancellationStopsExcludedDirectoryWalk(t *testing.T) {
	ing := newTestIngester(t)
	src := t.TempDir()
	for i := range 130 {
		require.NoError(t, os.MkdirAll(
			filepath.Join(src, fmt.Sprintf("%03d", i), "skip"), 0o700))
	}

	ctx, cancel := context.WithCancel(t.Context())
	var events []ProgressEvent
	_, err := ing.AddPathsWithOptions(ctx, []string{src}, "/inbox", Options{
		Exclude: []string{"skip"},
		Progress: func(event ProgressEvent) {
			events = append(events, event)
			if event.Excluded >= progressFileInterval {
				cancel()
			}
		},
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NotEmpty(t, events)
	assert.False(t, events[len(events)-1].Final)
	assert.Less(t, events[len(events)-1].Excluded, 130,
		"the walk must stop instead of traversing every excluded directory")
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

func TestPackedDuplicateIngestRemovesLooseCopy(t *testing.T) {
	ing := newTestIngesterWithOptions(t, blob.ManagedOptions())
	content := strings.Repeat("packed duplicate content\n", 512)
	src := writeTree(t, map[string]string{"document.txt": content})
	path := filepath.Join(src, "document.txt")

	first, err := ing.AddPaths(t.Context(), []string{path}, "/inbox")
	require.NoError(t, err)
	require.Equal(t, 1, first.Added)
	packed, err := ing.Blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, packed.BlobsPacked)
	loose, err := ing.Blobs.List()
	require.NoError(t, err)
	require.Empty(t, loose)

	second, err := ing.AddPaths(t.Context(), []string{path}, "/inbox")
	require.NoError(t, err)
	assert.Equal(t, 1, second.Skipped)
	loose, err = ing.Blobs.List()
	require.NoError(t, err)
	assert.Empty(t, loose)
}

func TestRejectedUploadPreservesAuthorityFreeAndRemovesPackedDuplicateLooseFiles(t *testing.T) {
	content := []byte(strings.Repeat("rejected upload content\n", 512))
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	wrong := strings.Repeat("0", 64)

	t.Run("authority free waits for GC", func(t *testing.T) {
		ing := newTestIngesterWithOptions(t, blob.ManagedOptions())
		_, err := ing.PrepareUpload(
			t.Context(), ing.Store.RootID(), "rejected.txt", "text/plain",
			bytes.NewReader(content), wrong, int64(len(content)),
		)
		require.ErrorIs(t, err, ErrUploadDigestMismatch)
		loose, err := ing.Blobs.List()
		require.NoError(t, err)
		assert.Len(t, loose, 1)
		assert.Contains(t, loose, hash)
	})

	t.Run("prepared envelope abandoned", func(t *testing.T) {
		ing := newTestIngesterWithOptions(t, blob.ManagedOptions())
		prepared, err := ing.PrepareUpload(
			t.Context(), ing.Store.RootID(), "abandoned.txt", "text/plain",
			bytes.NewReader(content), hash, int64(len(content)),
		)
		require.NoError(t, err)
		require.NoError(t, prepared.Discard())
		loose, err := ing.Blobs.List()
		require.NoError(t, err)
		assert.Len(t, loose, 1)
		assert.Contains(t, loose, hash)
	})

	t.Run("packed duplicate", func(t *testing.T) {
		ing := newTestIngesterWithOptions(t, blob.ManagedOptions())
		prepared, err := ing.PrepareUpload(
			t.Context(), ing.Store.RootID(), "packed.txt", "text/plain",
			bytes.NewReader(content), hash, int64(len(content)),
		)
		require.NoError(t, err)
		_, err = prepared.Commit(t.Context())
		require.NoError(t, err)
		packed, err := ing.Blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
		require.NoError(t, err)
		require.Equal(t, 1, packed.BlobsPacked)

		_, err = ing.PrepareUpload(
			t.Context(), ing.Store.RootID(), "rejected.txt", "text/plain",
			bytes.NewReader(content), wrong, int64(len(content)),
		)
		require.ErrorIs(t, err, ErrUploadDigestMismatch)
		loose, err := ing.Blobs.List()
		require.NoError(t, err)
		assert.Empty(t, loose)
	})
}

func TestRejectedUploadCannotDeleteAnotherPreparedUpload(t *testing.T) {
	ing := newTestIngesterWithOptions(t, blob.ManagedOptions())
	content := []byte(strings.Repeat("shared in-flight content\n", 512))
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	valid, err := ing.PrepareUpload(
		t.Context(), ing.Store.RootID(), "valid.txt", "text/plain",
		bytes.NewReader(content), hash, int64(len(content)),
	)
	require.NoError(t, err)
	_, err = ing.PrepareUpload(
		t.Context(), ing.Store.RootID(), "rejected.txt", "text/plain",
		bytes.NewReader(content), strings.Repeat("0", 64), int64(len(content)),
	)
	require.ErrorIs(t, err, ErrUploadDigestMismatch)

	result, err := valid.Commit(t.Context())
	require.NoError(t, err)
	assert.True(t, result.Added)
	assert.Equal(t, hash, result.Node.BlobHash)
	contentReader, err := ing.Blobs.Open(hash)
	require.NoError(t, err)
	defer func() { require.NoError(t, contentReader.Close()) }()
	got, err := io.ReadAll(contentReader)
	require.NoError(t, err)
	assert.Equal(t, content, got)
}

func TestMutationCleanupNeverMasksCommittedSuccess(t *testing.T) {
	cleanupErr := errors.New("cleanup failed")
	require.NoError(t, mutationCleanupResult(nil, cleanupErr))
	mutationErr := errors.New("mutation failed")
	require.ErrorIs(t, mutationCleanupResult(mutationErr, cleanupErr), mutationErr)
	assert.ErrorIs(t, mutationCleanupResult(mutationErr, cleanupErr), cleanupErr)
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
	require.NoError(t, ing.addTree(ctx, &rep, ingestID, dest.ID, src, src, exclusions{}, nil))
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

func TestPreflightPreservesPathFormExclusions(t *testing.T) {
	src := writeTree(t, map[string]string{
		"cache/root.bin":           "root cache",
		"project/cache/nested.bin": "nested cache",
		"keep.bin":                 "keep",
	})
	for _, rule := range []string{"cache/", "./cache"} {
		t.Run(rule, func(t *testing.T) {
			report, err := Preflight(t.Context(), []string{src}, Options{Exclude: []string{rule}})
			require.NoError(t, err)
			assert.Equal(t, int64(2), report.Files,
				"path-form rule must not become a basename match at every depth")
			assert.Equal(t, int64(1), report.Excluded)
			require.Len(t, report.Findings, 1)
			assert.Equal(t, filepath.Join(src, "cache"), report.Findings[0].Path)
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
