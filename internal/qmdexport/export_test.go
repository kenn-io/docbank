package qmdexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/kit/safefileio"
)

func TestPublishBuildsDeterministicOpaqueQMDGeneration(t *testing.T) {
	first, firstMarkdown := source(9, "00000000-0000-4000-8000-000000000009", "# Alpha\nsearchable alpha\n")
	second, secondMarkdown := source(2, "00000000-0000-4000-8000-000000000002", "# Beta\nsearchable beta\n")
	reader := fakeReader{first.BlobSHA256: firstMarkdown, second.BlobSHA256: secondMarkdown}
	root := privateExportTestDir(t)

	receipt, err := Publish(t.Context(), root, "docbank", []Source{first, second}, reader, Options{})
	require.NoError(t, err)
	assert.Len(t, receipt.Manifest.Entries, 2)
	assert.Equal(t, receipt.GenerationID, strings.TrimSpace(readFile(t, filepath.Join(root, "CURRENT"))))
	assert.Equal(t, filepath.Join(root, "generations", receipt.GenerationID, "collection"), receipt.CollectionPath)

	manifestBytes := []byte(readFile(t, filepath.Join(root, "generations", receipt.GenerationID, "manifest.json")))
	var manifest Manifest
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest, json.RejectUnknownMembers(true)))
	assert.Equal(t, receipt.Manifest, manifest)
	assert.True(t, slices.IsSortedFunc(manifest.Entries, func(a, b Entry) int { return strings.Compare(a.URI, b.URI) }))
	for _, entry := range manifest.Entries {
		assert.True(t, strings.HasPrefix(entry.URI, "qmd://docbank/documents/"))
		assert.NotContains(t, entry.URI, "Alpha")
		assert.NotContains(t, entry.URI, "Beta")
		assert.Equal(t, "00000000-0000-4000-8000-000000000001", entry.VaultUID)
		assert.NotEmpty(t, entry.NodeID)
		assert.NotEmpty(t, entry.ContentVersionID)
		assert.NotEmpty(t, entry.AttachmentID)
		assert.NotEmpty(t, entry.BuildID)
		assert.NotEmpty(t, entry.ArtifactChecksum)
	}

	beta := manifest.Entries[0]
	if beta.NodeID != 2 {
		beta = manifest.Entries[1]
	}
	exported := readFile(t, filepath.Join(receipt.CollectionPath, filepath.FromSlash(strings.TrimPrefix(beta.URI, "qmd://docbank/"))))
	assert.Equal(t, "# Beta\nsearchable beta\n", exported)

	reversed, err := Publish(t.Context(), root, "docbank", []Source{second, first}, reader, Options{})
	require.NoError(t, err)
	assert.Equal(t, receipt.GenerationID, reversed.GenerationID)
	assert.Equal(t, receipt.Manifest, reversed.Manifest)
}

func TestPublishReplacesCurrentAndRetainsPriorGeneration(t *testing.T) {
	root := privateExportTestDir(t)
	first, firstMarkdown := source(1, "00000000-0000-4000-8000-000000000001", "# One\n")
	firstReceipt, err := Publish(t.Context(), root, "docbank", []Source{first}, fakeReader{first.BlobSHA256: firstMarkdown}, Options{})
	require.NoError(t, err)
	second, secondMarkdown := source(2, "00000000-0000-4000-8000-000000000002", "# Two\n")
	secondReceipt, err := Publish(t.Context(), root, "docbank", []Source{second}, fakeReader{second.BlobSHA256: secondMarkdown}, Options{})
	require.NoError(t, err)
	assert.NotEqual(t, firstReceipt.GenerationID, secondReceipt.GenerationID)
	assert.Equal(t, string(firstMarkdown), readFile(t, filepath.Join(firstReceipt.CollectionPath, firstReceipt.Manifest.Entries[0].RelativePath)))
	assert.Equal(t, secondReceipt.GenerationID, strings.TrimSpace(readFile(t, filepath.Join(root, "CURRENT"))))
}

func TestPublishFailureKeepsPriorGenerationCurrent(t *testing.T) {
	root := privateExportTestDir(t)
	valid, validMarkdown := source(1, "00000000-0000-4000-8000-000000000001", "# One\n")
	receipt, err := Publish(t.Context(), root, "docbank", []Source{valid}, fakeReader{valid.BlobSHA256: validMarkdown}, Options{})
	require.NoError(t, err)
	invalid, _ := source(2, "00000000-0000-4000-8000-000000000002", "# Changed\n")
	reader := fakeReader{invalid.BlobSHA256: []byte("# Forged!\n")}
	_, err = Publish(t.Context(), root, "docbank", []Source{invalid}, reader, Options{})
	require.ErrorContains(t, err, "checksum")
	assert.Equal(t, receipt.GenerationID, strings.TrimSpace(readFile(t, filepath.Join(root, "CURRENT"))))
	stages, err := filepath.Glob(filepath.Join(root, "generations", ".stage-*"))
	require.NoError(t, err)
	assert.Empty(t, stages)
}

func TestPublishRejectsCorruptedExistingGeneration(t *testing.T) {
	root := privateExportTestDir(t)
	item, markdown := source(1, "00000000-0000-4000-8000-000000000001", "# One\n")
	receipt, err := Publish(t.Context(), root, "docbank", []Source{item}, fakeReader{item.BlobSHA256: markdown}, Options{})
	require.NoError(t, err)
	document := filepath.Join(receipt.CollectionPath, filepath.FromSlash(receipt.Manifest.Entries[0].RelativePath))
	require.NoError(t, os.WriteFile(document, []byte("# Two\n"), 0o600))

	_, err = Publish(t.Context(), root, "docbank", []Source{item}, fakeReader{item.BlobSHA256: markdown}, Options{})
	require.ErrorContains(t, err, "generation")
}

func TestPublishRejectsSymlinkedExistingCollection(t *testing.T) {
	root := privateExportTestDir(t)
	item, markdown := source(1, "00000000-0000-4000-8000-000000000001", "# One\n")
	receipt, err := Publish(t.Context(), root, "docbank", []Source{item}, fakeReader{item.BlobSHA256: markdown}, Options{})
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(receipt.CollectionPath))
	target := privateExportTestDir(t)
	if err := os.Symlink(target, receipt.CollectionPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err = Publish(t.Context(), root, "docbank", []Source{item}, fakeReader{item.BlobSHA256: markdown}, Options{})
	require.ErrorContains(t, err, "generation")
}

func TestPublishSerializesCurrentSelection(t *testing.T) {
	root := privateExportTestDir(t)
	first, firstMarkdown := source(1, "00000000-0000-4000-8000-000000000001", "# One\n")
	second, secondMarkdown := source(2, "00000000-0000-4000-8000-000000000002", "# Two\n")
	firstCurrent := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondWaiting := make(chan struct{})
	firstResult := make(chan publishResult, 1)
	secondResult := make(chan publishResult, 1)

	go func() {
		receipt, err := publish(t.Context(), root, "docbank", []Source{first}, fakeReader{first.BlobSHA256: firstMarkdown}, Options{}, publishHooks{
			afterCurrent: func() {
				close(firstCurrent)
				<-releaseFirst
			},
		})
		firstResult <- publishResult{receipt: receipt, err: err}
	}()
	<-firstCurrent
	go func() {
		receipt, err := publish(t.Context(), root, "docbank", []Source{second}, fakeReader{second.BlobSHA256: secondMarkdown}, Options{}, publishHooks{
			waitingOnLock: func() { close(secondWaiting) },
		})
		secondResult <- publishResult{receipt: receipt, err: err}
	}()
	<-secondWaiting
	close(releaseFirst)
	firstPublished := <-firstResult
	secondPublished := <-secondResult
	require.NoError(t, firstPublished.err)
	require.NoError(t, secondPublished.err)
	current := strings.TrimSpace(readFile(t, filepath.Join(root, "CURRENT")))
	assert.Equal(t, secondPublished.receipt.GenerationID, current)
	assert.DirExists(t, secondPublished.receipt.CollectionPath)
}

type publishResult struct {
	receipt Receipt
	err     error
}

func TestPublishEmptyActiveSetRetainsPriorGeneration(t *testing.T) {
	root := privateExportTestDir(t)
	item, markdown := source(1, "00000000-0000-4000-8000-000000000001", "# One\n")
	prior, err := Publish(t.Context(), root, "docbank", []Source{item}, fakeReader{item.BlobSHA256: markdown}, Options{})
	require.NoError(t, err)

	empty, err := PublishActive(t.Context(), root, "docbank", staticCatalog{}, fakeReader{}, Options{})
	require.NoError(t, err)
	assert.Empty(t, empty.Manifest.Entries)
	assert.DirExists(t, empty.CollectionPath)
	assert.Equal(t, empty.GenerationID, strings.TrimSpace(readFile(t, filepath.Join(root, "CURRENT"))))
	assert.Equal(t, string(markdown), readFile(t, filepath.Join(prior.CollectionPath, prior.Manifest.Entries[0].RelativePath)))
}

func TestPublishRejectsUnsafeCollectionAndBoundsBeforeReading(t *testing.T) {
	item, markdown := source(1, "00000000-0000-4000-8000-000000000001", "# One\n")
	reader := &countingReader{fakeReader: fakeReader{item.BlobSHA256: markdown}}
	_, err := Publish(t.Context(), privateExportTestDir(t), "../private", []Source{item}, reader, Options{})
	require.Error(t, err)
	assert.Zero(t, reader.opens)
	_, err = Publish(t.Context(), privateExportTestDir(t), "docbank", []Source{item}, reader, Options{MaxDocuments: 1, MaxDocumentBytes: 2, MaxTotalBytes: 2})
	require.ErrorContains(t, err, "bound")
	assert.Zero(t, reader.opens)

	item.ArtifactChecksum = strings.Repeat("f", 64)
	_, err = Publish(t.Context(), privateExportTestDir(t), "docbank", []Source{item}, reader, Options{})
	require.ErrorContains(t, err, "authority")
	assert.Zero(t, reader.opens)
}

func TestPublishRejectsNonUTF8Markdown(t *testing.T) {
	item, _ := source(1, "00000000-0000-4000-8000-000000000001", "placeholder")
	content := []byte{0xff, 0xfe}
	digest := sha256.Sum256(content)
	checksum := hex.EncodeToString(digest[:])
	item.BlobSHA256 = checksum
	item.MarkdownChecksum = checksum
	item.ArtifactChecksum = checksum
	item.BlobSize = int64(len(content))
	_, err := Publish(t.Context(), privateExportTestDir(t), "docbank", []Source{item}, fakeReader{checksum: content}, Options{})
	require.ErrorContains(t, err, "UTF-8")
}

type fakeReader map[string][]byte

type staticCatalog []Source

func (catalog staticCatalog) QMDExportSources(_ context.Context, limit int) ([]Source, error) {
	if len(catalog) > limit {
		return nil, errors.New("membership exceeds limit")
	}
	return slices.Clone(catalog), nil
}

func (reader fakeReader) OpenStreamContext(_ context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	content, ok := reader[hash]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return &verifiedReader{Reader: bytes.NewReader(content)}, int64(len(content)), nil
}

type countingReader struct {
	fakeReader

	opens int
}

func (reader *countingReader) OpenStreamContext(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	reader.opens++
	return reader.fakeReader.OpenStreamContext(ctx, hash)
}

type verifiedReader struct {
	io.Reader

	verified bool
}

func (reader *verifiedReader) Close() error   { return nil }
func (reader *verifiedReader) Verify() error  { reader.verified = true; return nil }
func (reader *verifiedReader) Verified() bool { return reader.verified }

func source(nodeID int64, versionID, markdown string) (Source, []byte) {
	digest := sha256.Sum256([]byte(markdown))
	checksum := hex.EncodeToString(digest[:])
	return Source{
		VaultUID: "00000000-0000-4000-8000-000000000001", NodeID: nodeID,
		ContentVersionID: versionID, ProcessingProfileFingerprint: strings.Repeat("a", 64),
		AttachmentID: "attachment-" + versionID, BuildID: strings.Repeat("b", 64),
		ArtifactID: "markdown", BlobSHA256: checksum, BlobSize: int64(len(markdown)),
		ArtifactChecksum: checksum, MarkdownChecksum: checksum,
	}, []byte(markdown)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(content)
}

var _ io.ReadCloser = (*verifiedReader)(nil)

func TestPublishStreamsIntoStageBeforeSourceEOF(t *testing.T) {
	root := privateExportTestDir(t)
	item, markdown := source(1, "version-1", strings.Repeat("€", 64<<10))
	var input io.Reader = bytes.NewReader(markdown)
	readBytes := 0
	reader := blobReaderFunc(func(context.Context, string) (packstore.VerifiedReadCloser, int64, error) {
		return &verifiedReader{Reader: readerFunc(func(p []byte) (int, error) {
			if readBytes >= 32<<10 {
				paths, err := filepath.Glob(filepath.Join(root, "generations", ".stage-*", "collection", "documents", "*", "*.md"))
				if err != nil || len(paths) != 1 {
					return 0, errors.New("document has not been staged while reading")
				}
				info, err := os.Stat(paths[0])
				if err != nil || info.Size() == 0 {
					return 0, errors.New("document bytes have not been staged while reading")
				}
			}
			n, err := input.Read(p)
			readBytes += n
			return n, err
		})}, int64(len(markdown)), nil
	})
	receipt, err := Publish(t.Context(), root, "docbank", []Source{item}, reader, Options{})
	require.NoError(t, err)
	assert.Equal(t, string(markdown), readFile(t, filepath.Join(receipt.CollectionPath, receipt.Manifest.Entries[0].RelativePath)))
}

func TestPublishPreservesReadError(t *testing.T) {
	item, _ := source(1, "version-1", "# One\n")
	reader := blobReaderFunc(func(context.Context, string) (packstore.VerifiedReadCloser, int64, error) {
		return &verifiedReader{Reader: readerFunc(func([]byte) (int, error) {
			return 0, io.ErrUnexpectedEOF
		})}, item.BlobSize, nil
	})
	_, err := Publish(t.Context(), privateExportTestDir(t), "docbank", []Source{item}, reader, Options{})
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestPublishRejectsAggregateBytesBeforeReading(t *testing.T) {
	first, markdown := source(1, "version-1", "# One\n")
	second, _ := source(2, "version-2", string(markdown))
	reader := &countingReader{fakeReader: fakeReader{first.BlobSHA256: markdown}}
	_, err := Publish(t.Context(), privateExportTestDir(t), "docbank", []Source{first, second}, reader, Options{MaxTotalBytes: first.BlobSize})
	require.ErrorContains(t, err, "bound")
	assert.Zero(t, reader.opens)
}

func TestPublishCleansAbandonedStage(t *testing.T) {
	root := privateExportTestDir(t)
	stage := filepath.Join(root, "generations", ".stage-abandoned")
	require.NoError(t, safefileio.EnsurePrivateDir(filepath.Dir(stage)))
	require.NoError(t, safefileio.EnsurePrivateDir(stage))
	require.NoError(t, os.WriteFile(filepath.Join(stage, "partial.md"), []byte("partial"), 0o600))
	item, markdown := source(1, "version-1", "# One\n")
	_, err := Publish(t.Context(), root, "docbank", []Source{item}, fakeReader{item.BlobSHA256: markdown}, Options{})
	require.NoError(t, err)
	_, err = os.Stat(stage)
	require.ErrorIs(t, err, os.ErrNotExist)
}

type readerFunc func([]byte) (int, error)

func (read readerFunc) Read(p []byte) (int, error) { return read(p) }

type blobReaderFunc func(context.Context, string) (packstore.VerifiedReadCloser, int64, error)

func (read blobReaderFunc) OpenStreamContext(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	return read(ctx, hash)
}

func privateExportTestDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, safefileio.EnsurePrivateDir(root))
	return root
}

func TestPublishCancellationKeepsCurrent(t *testing.T) {
	for _, phase := range []string{"empty", "final source", "before CURRENT"} {
		t.Run(phase, func(t *testing.T) {
			root := privateExportTestDir(t)
			priorSource, priorBody := source(1, "version-1", "# Prior\n")
			prior, err := Publish(t.Context(), root, "docbank", []Source{priorSource}, fakeReader{priorSource.BlobSHA256: priorBody}, Options{})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			next, body := source(2, "version-2", "# Next\n")
			var sources []Source
			var reader BlobReader = fakeReader{}
			switch phase {
			case "empty":
				cancel()
			case "final source":
				sources = []Source{next}
				var input io.Reader = bytes.NewReader(body)
				reader = blobReaderFunc(func(context.Context, string) (packstore.VerifiedReadCloser, int64, error) {
					return &verifiedReader{Reader: readerFunc(func(p []byte) (int, error) {
						n, err := input.Read(p)
						if errors.Is(err, io.EOF) {
							cancel()
						}
						return n, err
					})}, int64(len(body)), nil
				})
			case "before CURRENT":
				sources = []Source{next}
				reader = fakeReader{next.BlobSHA256: body}
				syncDir := pack.SyncDir
				t.Cleanup(func() { pack.SyncDir = syncDir })
				pack.SyncDir = func(path string) error {
					if path == filepath.Join(root, "generations") {
						cancel()
					}
					return syncDir(path)
				}
			}
			_, err = Publish(ctx, root, "docbank", sources, reader, Options{})
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, prior.GenerationID+"\n", readFile(t, filepath.Join(root, "CURRENT")))
		})
	}
}

func TestPublishReportsDirectorySyncFailures(t *testing.T) {
	for _, phase := range []string{"stage", "generation", "CURRENT"} {
		t.Run(phase, func(t *testing.T) {
			root := privateExportTestDir(t)
			first, body := source(1, "version-1", "# Prior\n")
			prior, err := Publish(t.Context(), root, "docbank", []Source{first}, fakeReader{first.BlobSHA256: body}, Options{})
			require.NoError(t, err)
			syncErr := errors.New("directory sync failed")
			syncDir := pack.SyncDir
			t.Cleanup(func() { pack.SyncDir = syncDir })
			pack.SyncDir = func(path string) error {
				_, manifestErr := os.Stat(filepath.Join(path, "manifest.json"))
				if phase == "stage" && strings.Contains(path, ".stage-") && manifestErr == nil ||
					phase == "generation" && path == filepath.Join(root, "generations") ||
					phase == "CURRENT" && path == root {
					return syncErr
				}
				return syncDir(path)
			}
			next, body := source(2, "version-2", "# Next\n")
			_, err = Publish(t.Context(), root, "docbank", []Source{next}, fakeReader{next.BlobSHA256: body}, Options{})
			require.ErrorIs(t, err, syncErr)
			current := readFile(t, filepath.Join(root, "CURRENT"))
			if phase == "CURRENT" {
				assert.NotEqual(t, prior.GenerationID+"\n", current)
				assert.DirExists(t, filepath.Join(root, "generations", strings.TrimSpace(current)))
			} else {
				assert.Equal(t, prior.GenerationID+"\n", current)
			}
		})
	}
}

func TestPublishRejectsBroadPermissions(t *testing.T) {
	for _, target := range []string{"root", "generations", "generation", "collection", "documents", "shard", "document", "manifest", "lock", "CURRENT"} {
		t.Run(target, func(t *testing.T) {
			root := filepath.Join(privateExportTestDir(t), "export")
			item, body := source(1, "version-1", "# Private synthetic document\n")
			reader := fakeReader{item.BlobSHA256: body}
			receipt, err := Publish(t.Context(), root, "docbank", []Source{item}, reader, Options{})
			require.NoError(t, err)
			generation := filepath.Dir(receipt.CollectionPath)
			document := filepath.Join(receipt.CollectionPath, filepath.FromSlash(receipt.Manifest.Entries[0].RelativePath))
			paths := map[string]string{
				"root": root, "generations": filepath.Dir(generation), "generation": generation,
				"collection": receipt.CollectionPath, "documents": filepath.Join(receipt.CollectionPath, "documents"),
				"shard": filepath.Dir(document), "document": document, "manifest": filepath.Join(generation, "manifest.json"),
				"lock": filepath.Join(root, ".publish.lock"), "CURRENT": filepath.Join(root, "CURRENT"),
			}
			broadenExportPermissions(t, paths[target])
			_, err = Publish(t.Context(), root, "docbank", []Source{item}, reader, Options{})
			require.Error(t, err)
			assert.Equal(t, receipt.GenerationID+"\n", readFile(t, filepath.Join(root, "CURRENT")))
		})
	}
}

func TestPublishSyncsDirectoriesBeforeSelectingGeneration(t *testing.T) {
	root := privateExportTestDir(t)
	item, body := source(1, "version-1", "# One\n")
	var synced []string
	syncDir := pack.SyncDir
	t.Cleanup(func() { pack.SyncDir = syncDir })
	pack.SyncDir = func(path string) error {
		stages, err := filepath.Glob(filepath.Join(root, "generations", ".stage-*", "manifest.json"))
		require.NoError(t, err)
		if len(stages) == 1 {
			relative, err := filepath.Rel(filepath.Dir(stages[0]), path)
			require.NoError(t, err)
			synced = append(synced, filepath.ToSlash(relative))
		} else if len(synced) > 0 {
			relative, err := filepath.Rel(root, path)
			require.NoError(t, err)
			synced = append(synced, filepath.ToSlash(relative))
			_, err = os.Stat(filepath.Join(root, "CURRENT"))
			if path == root {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		}
		return syncDir(path)
	}
	_, err := Publish(t.Context(), root, "docbank", []Source{item}, fakeReader{item.BlobSHA256: body}, Options{})
	require.NoError(t, err)
	assert.Equal(t, []string{"collection/documents/" + sourcePathIdentity(item)[:2], "collection/documents", "collection", ".", "generations", "."}, synced)
}
