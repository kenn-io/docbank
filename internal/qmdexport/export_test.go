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

	"go.kenn.io/kit/packstore"
)

func TestPublishBuildsDeterministicOpaqueQMDGeneration(t *testing.T) {
	first, firstMarkdown := source(9, "00000000-0000-4000-8000-000000000009", "# Alpha\nsearchable alpha\n")
	second, secondMarkdown := source(2, "00000000-0000-4000-8000-000000000002", "---\nprivate_tag: hidden-taxonomy\n---\n# Beta\nsearchable beta\n")
	reader := fakeReader{first.BlobSHA256: firstMarkdown, second.BlobSHA256: secondMarkdown}
	root := t.TempDir()

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
	assert.NotContains(t, exported, "hidden-taxonomy")
	assert.Contains(t, beta.Frontmatter, "private_tag: hidden-taxonomy")

	reversed, err := Build(t.Context(), "docbank", []Source{second, first}, reader, Options{})
	require.NoError(t, err)
	assert.Equal(t, receipt.GenerationID, reversed.ID)
	assert.Equal(t, receipt.Manifest, reversed.Manifest)
}

func TestLoadCurrentReturnsOnlySelfConsistentManifest(t *testing.T) {
	root := t.TempDir()
	item, markdown := source(1, "00000000-0000-4000-8000-000000000001", "# One\n")
	published, err := Publish(t.Context(), root, "docbank", []Source{item}, fakeReader{item.BlobSHA256: markdown}, Options{})
	require.NoError(t, err)

	loaded, err := LoadCurrent(root)
	require.NoError(t, err)
	assert.Equal(t, published.GenerationID, loaded.GenerationID)
	assert.Equal(t, published.Manifest, loaded.Manifest)
	assert.Equal(t, published.CollectionPath, loaded.CollectionPath)

	manifestPath := filepath.Join(root, "generations", published.GenerationID, "manifest.json")
	manifest := published.Manifest
	manifest.Entries[0].AttachmentID = "drifted"
	encoded, err := json.Marshal(manifest, json.Deterministic(true))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, append(encoded, '\n'), 0o600))
	_, err = LoadCurrent(root)
	require.ErrorContains(t, err, "identity")
}

func TestLoadCurrentRejectsUnsafePointer(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "CURRENT"), []byte("../private\n"), 0o600))
	_, err := LoadCurrent(root)
	require.ErrorContains(t, err, "CURRENT")
	volumeRoot := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	_, err = LoadCurrent(volumeRoot)
	require.ErrorContains(t, err, "root")
}

func TestPublishReplacesCurrentAndRemovesStaleGeneration(t *testing.T) {
	root := t.TempDir()
	first, firstMarkdown := source(1, "00000000-0000-4000-8000-000000000001", "# One\n")
	firstReceipt, err := Publish(t.Context(), root, "docbank", []Source{first}, fakeReader{first.BlobSHA256: firstMarkdown}, Options{})
	require.NoError(t, err)
	second, secondMarkdown := source(2, "00000000-0000-4000-8000-000000000002", "# Two\n")
	secondReceipt, err := Publish(t.Context(), root, "docbank", []Source{second}, fakeReader{second.BlobSHA256: secondMarkdown}, Options{})
	require.NoError(t, err)
	assert.NotEqual(t, firstReceipt.GenerationID, secondReceipt.GenerationID)
	_, err = os.Stat(filepath.Join(root, "generations", firstReceipt.GenerationID))
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, secondReceipt.GenerationID, strings.TrimSpace(readFile(t, filepath.Join(root, "CURRENT"))))
}

func TestPublishFailureKeepsPriorGenerationCurrent(t *testing.T) {
	root := t.TempDir()
	valid, validMarkdown := source(1, "00000000-0000-4000-8000-000000000001", "# One\n")
	receipt, err := Publish(t.Context(), root, "docbank", []Source{valid}, fakeReader{valid.BlobSHA256: validMarkdown}, Options{})
	require.NoError(t, err)
	invalid, _ := source(2, "00000000-0000-4000-8000-000000000002", "# Changed\n")
	reader := fakeReader{invalid.BlobSHA256: []byte("# Forged!\n")}
	_, err = Publish(t.Context(), root, "docbank", []Source{invalid}, reader, Options{})
	require.ErrorContains(t, err, "checksum")
	assert.Equal(t, receipt.GenerationID, strings.TrimSpace(readFile(t, filepath.Join(root, "CURRENT"))))
}

func TestPublishRejectsCorruptedExistingGeneration(t *testing.T) {
	root := t.TempDir()
	item, markdown := source(1, "00000000-0000-4000-8000-000000000001", "# One\n")
	receipt, err := Publish(t.Context(), root, "docbank", []Source{item}, fakeReader{item.BlobSHA256: markdown}, Options{})
	require.NoError(t, err)
	document := filepath.Join(receipt.CollectionPath, filepath.FromSlash(receipt.Manifest.Entries[0].RelativePath))
	require.NoError(t, os.WriteFile(document, []byte("# Two\n"), 0o600))

	_, err = Publish(t.Context(), root, "docbank", []Source{item}, fakeReader{item.BlobSHA256: markdown}, Options{})
	require.ErrorContains(t, err, "generation")
}

func TestPublishRejectsSymlinkedExistingCollection(t *testing.T) {
	root := t.TempDir()
	item, markdown := source(1, "00000000-0000-4000-8000-000000000001", "# One\n")
	receipt, err := Publish(t.Context(), root, "docbank", []Source{item}, fakeReader{item.BlobSHA256: markdown}, Options{})
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(receipt.CollectionPath))
	target := t.TempDir()
	if err := os.Symlink(target, receipt.CollectionPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err = Publish(t.Context(), root, "docbank", []Source{item}, fakeReader{item.BlobSHA256: markdown}, Options{})
	require.ErrorContains(t, err, "generation")
}

func TestPublishSerializesCurrentSelectionAndStaleCleanup(t *testing.T) {
	root := t.TempDir()
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

func TestPublishEmptyActiveSetRemovesLastStaleGeneration(t *testing.T) {
	root := t.TempDir()
	item, markdown := source(1, "00000000-0000-4000-8000-000000000001", "# One\n")
	prior, err := Publish(t.Context(), root, "docbank", []Source{item}, fakeReader{item.BlobSHA256: markdown}, Options{})
	require.NoError(t, err)

	empty, err := PublishActive(t.Context(), root, "docbank", staticCatalog{}, fakeReader{}, Options{})
	require.NoError(t, err)
	assert.Empty(t, empty.Manifest.Entries)
	assert.DirExists(t, empty.CollectionPath)
	assert.Equal(t, empty.GenerationID, strings.TrimSpace(readFile(t, filepath.Join(root, "CURRENT"))))
	_, err = os.Stat(filepath.Join(root, "generations", prior.GenerationID))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestBuildRejectsUnsafeCollectionAndBoundsBeforeReading(t *testing.T) {
	item, markdown := source(1, "00000000-0000-4000-8000-000000000001", "# One\n")
	reader := &countingReader{fakeReader: fakeReader{item.BlobSHA256: markdown}}
	_, err := Build(t.Context(), "../private", []Source{item}, reader, Options{})
	require.Error(t, err)
	assert.Zero(t, reader.opens)
	_, err = Build(t.Context(), "docbank", []Source{item}, reader, Options{MaxDocuments: 1, MaxDocumentBytes: 2, MaxTotalBytes: 2})
	require.ErrorContains(t, err, "bound")
	assert.Zero(t, reader.opens)

	item.ArtifactChecksum = strings.Repeat("f", 64)
	_, err = Build(t.Context(), "docbank", []Source{item}, reader, Options{})
	require.ErrorContains(t, err, "authority")
	assert.Zero(t, reader.opens)
}

func TestBuildRejectsNonUTF8Markdown(t *testing.T) {
	item, _ := source(1, "00000000-0000-4000-8000-000000000001", "placeholder")
	content := []byte{0xff, 0xfe}
	digest := sha256.Sum256(content)
	checksum := hex.EncodeToString(digest[:])
	item.BlobSHA256 = checksum
	item.MarkdownChecksum = checksum
	item.ArtifactChecksum = checksum
	item.BlobSize = int64(len(content))
	_, err := Build(t.Context(), "docbank", []Source{item}, fakeReader{checksum: content}, Options{})
	require.ErrorContains(t, err, "UTF-8")
}

func TestBuildRejectsManifestAbovePublicationBound(t *testing.T) {
	item, markdown := source(1, "00000000-0000-4000-8000-000000000001", "---\nprivate: "+strings.Repeat("x", 256)+"\n---\nbody\n")
	_, err := Build(t.Context(), "docbank", []Source{item}, fakeReader{item.BlobSHA256: markdown}, Options{MaxManifestBytes: 128})
	require.ErrorContains(t, err, "manifest")
}

func TestSplitFrontmatterUsesEarliestValidTerminator(t *testing.T) {
	tests := []struct {
		name        string
		markdown    string
		frontmatter string
		body        string
	}{
		{
			name:        "YAML document end precedes later fence",
			markdown:    "---\ntitle: One\n...\nsearchable\n---\ntail\n",
			frontmatter: "title: One",
			body:        "searchable\n---\ntail\n",
		},
		{
			name:        "closing fence precedes later YAML document end",
			markdown:    "---\ntitle: Two\n---\nsearchable\n...\ntail\n",
			frontmatter: "title: Two",
			body:        "searchable\n...\ntail\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frontmatter, body := splitFrontmatter([]byte(test.markdown))
			assert.Equal(t, test.frontmatter, frontmatter)
			assert.Equal(t, test.body, string(body))
		})
	}
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
	*bytes.Reader

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
