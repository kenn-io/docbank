package docbank

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	docsqlite "go.kenn.io/docbank/pkg/sqlite"
	"go.kenn.io/docbank/pkg/sqlite/modernc"
)

func TestVaultCreateIsImmutableAndIdempotent(t *testing.T) {
	require := require.New(t)
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(vault.Close()) })

	content := []byte("immutable content\n")
	expected := contentIdentity(content)
	created, err := vault.Create(t.Context(), "/immutable.txt", bytes.NewReader(content), CreateOptions{
		MediaType: "text/plain", Expected: expected,
	})
	require.NoError(err)
	require.True(created.Created)
	require.False(created.Replaced)
	require.Equal(int64(1), created.Node.Revision)

	retry, err := vault.Create(t.Context(), "/immutable.txt", bytes.NewReader(content), CreateOptions{
		MediaType: "text/plain", Expected: expected,
	})
	require.NoError(err)
	require.False(retry.Created)
	require.False(retry.Replaced)
	require.Equal(created.Node.ID, retry.Node.ID)
	require.Equal(created.Node.Revision, retry.Node.Revision)
	require.Equal(created.Version.ID, retry.Version.ID)

	tests := []struct {
		name      string
		content   []byte
		mediaType string
		expected  ContentIdentity
	}{
		{name: "different bytes", content: []byte("different\n"), mediaType: "text/plain", expected: contentIdentity([]byte("different\n"))},
		{name: "different media type", content: content, mediaType: "application/octet-stream", expected: expected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := vault.Create(t.Context(), "/immutable.txt", bytes.NewReader(test.content), CreateOptions{
				MediaType: test.mediaType, Expected: test.expected,
			})
			require.ErrorIs(err, ErrContentConflict)
		})
	}

	versions, err := vault.Versions(t.Context(), created.Node.ID, VersionsOptions{})
	require.NoError(err)
	require.Equal(1, versions.Total)
	require.Len(versions.Items, 1)
	after, err := vault.Stat(t.Context(), "/immutable.txt")
	require.NoError(err)
	require.Equal(int64(1), after.Revision)
	require.Equal(created.Computed.SHA256, after.BlobHash)
}

func TestVaultCreateRejectsExpectedIdentityMismatch(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	content := []byte("authoritative content\n")
	expected := contentIdentity(content)
	other := contentIdentity([]byte("other content\n"))

	tests := []struct {
		name     string
		expected ContentIdentity
		wantErr  error
	}{
		{name: "digest", expected: ContentIdentity{SHA256: other.SHA256, Size: expected.Size}, wantErr: ErrDigestMismatch},
		{name: "size", expected: ContentIdentity{SHA256: expected.SHA256, Size: expected.Size + 1}, wantErr: ErrSizeMismatch},
		{name: "missing identity", expected: ContentIdentity{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := vault.Create(t.Context(), "/missing.txt", bytes.NewReader(content), CreateOptions{
				MediaType: "text/plain", Expected: test.expected,
			})
			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
			} else {
				require.Error(t, err)
			}
			_, statErr := vault.Stat(t.Context(), "/missing.txt")
			require.ErrorIs(t, statErr, ErrNotFound)
		})
	}
}

func TestVaultCreateConcurrent(t *testing.T) {
	t.Run("identical creators converge", func(t *testing.T) {
		vault, err := New(t.Context(), Config{Root: t.TempDir()})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, vault.Close()) })
		content := []byte("same concurrent content\n")
		expected := contentIdentity(content)
		results, errs := runConcurrentCreates(t, vault, []createAttempt{
			{content: content, expected: expected}, {content: content, expected: expected},
		})
		require.NoError(t, errs[0])
		require.NoError(t, errs[1])
		assert.Equal(t, results[0].Node.ID, results[1].Node.ID)
		assert.Equal(t, results[0].Version.ID, results[1].Version.ID)
		assert.ElementsMatch(t, []bool{true, false}, []bool{results[0].Created, results[1].Created})
		versions, err := vault.Versions(t.Context(), results[0].Node.ID, VersionsOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, versions.Total)
		assert.Equal(t, int64(1), results[0].Node.Revision)
	})

	t.Run("different creators produce one winner", func(t *testing.T) {
		vault, err := New(t.Context(), Config{Root: t.TempDir()})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, vault.Close()) })
		first := []byte("first concurrent content\n")
		second := []byte("second concurrent content\n")
		results, errs := runConcurrentCreates(t, vault, []createAttempt{
			{content: first, expected: contentIdentity(first)},
			{content: second, expected: contentIdentity(second)},
		})
		var winner int
		switch {
		case errs[0] == nil && errors.Is(errs[1], ErrContentConflict):
			winner = 0
		case errs[1] == nil && errors.Is(errs[0], ErrContentConflict):
			winner = 1
		default:
			require.Fail(t, "want exactly one success and one content conflict", "errors: %v", errs)
		}
		assert.True(t, results[winner].Created)
		assert.Equal(t, int64(1), results[winner].Node.Revision)
		versions, err := vault.Versions(t.Context(), results[winner].Node.ID, VersionsOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, versions.Total)
	})
}

func TestVaultRepairContentPreservesLogicalReferences(t *testing.T) {
	tests := []struct {
		name         string
		config       func(string) Config
		content      []byte
		pack         bool
		wantEncoding string
	}{
		{
			name: "raw", config: func(root string) Config { return Config{Root: root} },
			content: []byte("trusted raw content\n"), wantEncoding: "raw",
		},
		{
			name: "zstd", config: func(root string) Config {
				return Config{Root: root, LooseCompression: LooseCompressionOptions{
					Enabled: true, MinBytes: 1024, MinSavingsPercent: 10,
				}}
			},
			content: []byte(strings.Repeat("trusted compressed content\n", 256)), wantEncoding: "zstd",
		},
		{
			name: "packed", config: func(root string) Config { return Config{Root: root} },
			content: []byte("trusted packed content\n"), pack: true, wantEncoding: "raw",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := New(t.Context(), test.config(root))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })

			first, err := vault.Put(t.Context(), "/first.txt", bytes.NewReader(test.content),
				PutOptions{MediaType: "text/plain"})
			require.NoError(t, err)
			if test.pack {
				report, err := vault.Pack(t.Context(), PackOptions{})
				require.NoError(t, err)
				require.Equal(t, 1, report.BlobsPacked)
			}
			second, err := vault.Create(t.Context(), "/second.txt", bytes.NewReader(test.content),
				CreateOptions{MediaType: "text/plain", Expected: first.Computed})
			require.NoError(t, err)
			replacement := []byte("new current content\n")
			_, err = vault.Put(t.Context(), "/first.txt", bytes.NewReader(replacement),
				PutOptions{MediaType: "text/plain"})
			require.NoError(t, err)

			physicalBefore, err := vault.metadata.PhysicalContent(t.Context(), first.Computed.SHA256)
			require.NoError(t, err)
			corruptVaultBlob(t, root, first.Computed, physicalBefore.Kind)
			assertVaultContentCorrupt(t, vault, "/second.txt")

			_, err = vault.RepairContent(t.Context(), first.Computed, bytes.NewReader([]byte("wrong bytes")))
			require.ErrorIs(t, err, packstore.ErrContentMismatch)
			physicalAfterFailure, err := vault.metadata.PhysicalContent(t.Context(), first.Computed.SHA256)
			require.NoError(t, err)
			assert.Equal(t, physicalBefore, physicalAfterFailure)

			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = vault.RepairContent(ctx, first.Computed, bytes.NewReader(test.content))
			require.ErrorIs(t, err, context.Canceled)
			physicalAfterCancellation, err := vault.metadata.PhysicalContent(t.Context(), first.Computed.SHA256)
			require.NoError(t, err)
			assert.Equal(t, physicalBefore, physicalAfterCancellation)

			repaired, err := vault.RepairContent(t.Context(), first.Computed, bytes.NewReader(test.content))
			require.NoError(t, err)
			assert.Equal(t, first.Computed, repaired.Computed)
			assert.Equal(t, int64(2), repaired.ReferencesPreserved)
			assert.Equal(t, PhysicalContent{
				Kind: "loose", Encoding: test.wantEncoding,
				LogicalBytes: first.Computed.Size, StoredBytes: repaired.Physical.StoredBytes,
				PackEligible: true,
			}, repaired.Physical)

			assertVaultContent(t, vault, "/second.txt", test.content)
			historical, err := vault.OpenVersionContent(t.Context(), first.Version.ID)
			require.NoError(t, err)
			got, err := io.ReadAll(historical.Reader)
			require.NoError(t, err)
			assert.Equal(t, test.content, got)
			require.NoError(t, historical.Reader.Close())
			assertVaultContent(t, vault, "/first.txt", replacement)

			firstVersions, err := vault.Versions(t.Context(), first.Node.ID, VersionsOptions{})
			require.NoError(t, err)
			assert.Equal(t, 2, firstVersions.Total)
			secondVersions, err := vault.Versions(t.Context(), second.Node.ID, VersionsOptions{})
			require.NoError(t, err)
			assert.Equal(t, 1, secondVersions.Total)
		})
	}
}

func corruptVaultBlob(t *testing.T, root string, identity ContentIdentity, kind string) {
	t.Helper()
	if kind == "packed" {
		packs, err := filepath.Glob(filepath.Join(root, "blobs", "packs", "*", "*"+packstore.PackExt))
		require.NoError(t, err)
		require.Len(t, packs, 1)
		require.NoError(t, os.WriteFile(packs[0], []byte("corrupt pack"), 0o600))
		return
	}
	raw := filepath.Join(root, "blobs", identity.SHA256[:2], identity.SHA256)
	path := raw
	if _, err := os.Stat(raw + ".zst"); err == nil {
		path = raw + ".zst"
	}
	corrupt := bytes.Repeat([]byte{'x'}, int(identity.Size))
	require.NoError(t, os.WriteFile(path, corrupt, 0o600))
}

func assertVaultContentCorrupt(t *testing.T, vault *Vault, path string) {
	t.Helper()
	content, err := vault.OpenContent(t.Context(), path)
	if err != nil {
		return
	}
	_, readErr := io.ReadAll(content.Reader)
	closeErr := content.Reader.Close()
	assert.Error(t, errors.Join(readErr, closeErr))
}

func assertVaultContent(t *testing.T, vault *Vault, path string, want []byte) {
	t.Helper()
	content, err := vault.OpenContent(t.Context(), path)
	require.NoError(t, err)
	got, err := io.ReadAll(content.Reader)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	require.NoError(t, content.Reader.Close())
}

type createAttempt struct {
	content  []byte
	expected ContentIdentity
}

func runConcurrentCreates(
	t *testing.T, vault *Vault, attempts []createAttempt,
) ([]PutReceipt, []error) {
	t.Helper()
	results := make([]PutReceipt, len(attempts))
	errs := make([]error, len(attempts))
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(len(attempts))
	var done sync.WaitGroup
	done.Add(len(attempts))
	for i, attempt := range attempts {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			results[i], errs[i] = vault.Create(t.Context(), "/concurrent.txt",
				bytes.NewReader(attempt.content), CreateOptions{
					MediaType: "text/plain", Expected: attempt.expected,
				})
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	return results, errs
}

func contentIdentity(content []byte) ContentIdentity {
	sum := sha256.Sum256(content)
	return ContentIdentity{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))}
}

func TestNewConfiguresLooseCompression(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	vault, err := New(t.Context(), Config{
		Root: root,
		LooseCompression: LooseCompressionOptions{
			Enabled:           true,
			MinBytes:          1024,
			MinSavingsPercent: 10,
		},
	})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(vault.Close()) })

	content := strings.Repeat("compressible document content\n", 512)
	receipt, err := vault.Put(t.Context(), "/document.txt", strings.NewReader(content), PutOptions{})
	require.NoError(err)
	require.Equal(PhysicalContent{
		Kind: "loose", Encoding: "zstd", LogicalBytes: int64(len(content)),
		StoredBytes: receipt.Physical.StoredBytes, PackEligible: true,
	}, receipt.Physical)
	require.Less(receipt.Physical.StoredBytes, receipt.Physical.LogicalBytes)
	compressedPath := filepath.Join(root, "blobs", receipt.Computed.SHA256[:2], receipt.Computed.SHA256+".zst")
	require.FileExists(compressedPath)
	backlog, err := vault.LooseBacklog(t.Context())
	require.NoError(err)
	require.Equal(LooseBacklog{
		EligibleObjects: 1, EligibleBytes: int64(len(content)), CompressedObjects: 1,
	}, backlog)

	opened, err := vault.OpenContent(t.Context(), "/document.txt")
	require.NoError(err)
	got, err := io.ReadAll(opened.Reader)
	require.NoError(err)
	require.Equal(content, string(got))
	require.NoError(opened.Reader.Close())
}

func TestPutPackedDuplicateRemovesRedundantLoose(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	vault, err := New(t.Context(), Config{Root: root})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(vault.Close()) })

	content := "shared packed content\n"
	first, err := vault.Put(t.Context(), "/first.txt", strings.NewReader(content), PutOptions{})
	require.NoError(err)
	packed, err := vault.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	require.Equal(1, packed.BlobsPacked)

	second, err := vault.Put(t.Context(), "/second.txt", strings.NewReader(content), PutOptions{})
	require.NoError(err)
	require.Equal(first.Computed, second.Computed)
	require.Equal(PhysicalContent{
		Kind: "packed", Encoding: "raw", LogicalBytes: int64(len(content)),
		StoredBytes: second.Physical.StoredBytes, PackEligible: true,
	}, second.Physical)

	rawPath := filepath.Join(root, "blobs", first.Computed.SHA256[:2], first.Computed.SHA256)
	require.NoFileExists(rawPath)
	require.NoFileExists(rawPath + ".zst")
	backlog, err := vault.LooseBacklog(t.Context())
	require.NoError(err)
	require.Equal(LooseBacklog{}, backlog)
}

func TestPutPackedExpectedMismatchRemovesRedundantLoose(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	vault, err := New(t.Context(), Config{Root: root})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(vault.Close()) })

	content := "shared packed content\n"
	first, err := vault.Put(t.Context(), "/first.txt", strings.NewReader(content), PutOptions{})
	require.NoError(err)
	_, err = vault.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	other := sha256.Sum256([]byte("different content\n"))
	_, err = vault.Put(t.Context(), "/rejected.txt", strings.NewReader(content), PutOptions{
		Expected: &ContentIdentity{SHA256: hex.EncodeToString(other[:]), Size: int64(len(content))},
	})
	require.ErrorIs(err, ErrDigestMismatch)

	rawPath := filepath.Join(root, "blobs", first.Computed.SHA256[:2], first.Computed.SHA256)
	require.NoFileExists(rawPath)
	require.NoFileExists(rawPath + ".zst")
	_, err = vault.Stat(t.Context(), "/rejected.txt")
	require.ErrorIs(err, ErrNotFound)
}

func TestNewRejectsInvalidLooseCompressionPolicy(t *testing.T) {
	tests := []struct {
		name string
		opts LooseCompressionOptions
	}{
		{name: "negative minimum bytes", opts: LooseCompressionOptions{Enabled: true, MinBytes: -1}},
		{name: "negative savings", opts: LooseCompressionOptions{Enabled: true, MinSavingsPercent: -1}},
		{name: "savings above one hundred", opts: LooseCompressionOptions{Enabled: true, MinSavingsPercent: 101}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			vault, err := New(t.Context(), Config{Root: t.TempDir(), LooseCompression: test.opts})
			require.Error(t, err)
			require.Nil(t, vault)
		})
	}
}

func TestPutExpectedMismatchLeavesTreeUnchanged(t *testing.T) {
	content := []byte("authoritative bytes\n")
	actual := sha256.Sum256(content)
	other := sha256.Sum256([]byte("different bytes\n"))
	tests := []struct {
		name     string
		expected ContentIdentity
		wantErr  error
	}{
		{
			name: "size", expected: ContentIdentity{
				SHA256: hex.EncodeToString(actual[:]), Size: int64(len(content) + 1),
			}, wantErr: ErrSizeMismatch,
		},
		{
			name: "digest", expected: ContentIdentity{
				SHA256: hex.EncodeToString(other[:]), Size: int64(len(content)),
			}, wantErr: ErrDigestMismatch,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			vault, err := New(t.Context(), Config{Root: t.TempDir()})
			require.NoError(err)
			t.Cleanup(func() { require.NoError(vault.Close()) })

			before, err := vault.Stat(t.Context(), "/")
			require.NoError(err)
			_, err = vault.Put(t.Context(), "/missing/parent/file.bin", bytes.NewReader(content),
				PutOptions{Expected: &test.expected})
			require.ErrorIs(err, test.wantErr)

			after, err := vault.Stat(t.Context(), "/")
			require.NoError(err)
			require.Equal(before, after)
			_, err = vault.Stat(t.Context(), "/missing")
			require.ErrorIs(err, ErrNotFound)
			loose, err := vault.blobs.List()
			require.NoError(err)
			require.Empty(loose)
		})
	}
}

func TestPutMetadataFailureRemovesOnlyUnrecordedLooseBlob(t *testing.T) {
	require := require.New(t)
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(vault.Close()) })

	kept, err := vault.Put(t.Context(), "/existing/file.txt", strings.NewReader("kept\n"), PutOptions{})
	require.NoError(err)
	_, err = vault.Put(t.Context(), "/existing", strings.NewReader("kept\n"), PutOptions{})
	require.ErrorIs(err, ErrNotFile)
	content, err := vault.OpenContent(t.Context(), "/existing/file.txt")
	require.NoError(err)
	keptContent, err := io.ReadAll(content.Reader)
	require.NoError(err)
	require.NoError(content.Reader.Close())
	require.Equal("kept\n", string(keptContent))

	_, err = vault.Put(t.Context(), "/existing", strings.NewReader("orphan\n"), PutOptions{})
	require.ErrorIs(err, ErrNotFile)

	loose, err := vault.blobs.List()
	require.NoError(err)
	require.Equal(map[string]int64{kept.Computed.SHA256: kept.Computed.Size}, loose)
}

func TestChildrenReturnsBoundedStablePages(t *testing.T) {
	require := require.New(t)
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(vault.Close()) })

	for path, content := range map[string]string{
		"/manifests/zulu.json":         "zulu\n",
		"/manifests/alpha.json":        "alpha\n",
		"/manifests/nested/child.json": "nested\n",
	} {
		_, err := vault.Put(t.Context(), path, strings.NewReader(content), PutOptions{})
		require.NoError(err)
	}
	dir, err := vault.Stat(t.Context(), "/manifests")
	require.NoError(err)

	first, err := vault.Children(t.Context(), dir.ID, ChildrenOptions{Limit: 2})
	require.NoError(err)
	require.Len(first.Items, 2)
	require.Equal(3, first.Total)
	require.Equal(2, first.Limit)
	require.Zero(first.Offset)
	require.Equal([]string{"nested", "alpha.json"}, nodeNames(first.Items))
	require.Equal([]string{"dir", "file"}, []string{first.Items[0].Kind, first.Items[1].Kind})
	require.Zero(first.Items[0].Size)
	require.Equal(int64(6), first.Items[1].Size)
	require.NotEmpty(first.Items[1].CurrentVersionID)
	require.NotEmpty(first.Items[1].BlobHash)

	second, err := vault.Children(t.Context(), dir.ID, ChildrenOptions{Limit: 2, Offset: 2})
	require.NoError(err)
	require.Equal(3, second.Total)
	require.Equal(2, second.Limit)
	require.Equal(2, second.Offset)
	require.Equal([]string{"zulu.json"}, nodeNames(second.Items))

	empty, err := vault.Children(t.Context(), dir.ID, ChildrenOptions{Limit: 2, Offset: 3})
	require.NoError(err)
	require.Empty(empty.Items)
	require.Equal(3, empty.Total)
	require.Equal(3, empty.Offset)

	defaultPage, err := vault.Children(t.Context(), dir.ID, ChildrenOptions{})
	require.NoError(err)
	require.Equal(DefaultChildrenLimit, defaultPage.Limit)
	require.Equal([]string{"nested", "alpha.json", "zulu.json"}, nodeNames(defaultPage.Items))

	file, err := vault.Stat(t.Context(), "/manifests/alpha.json")
	require.NoError(err)
	_, err = vault.Children(t.Context(), file.ID, ChildrenOptions{})
	require.ErrorIs(err, ErrNotDirectory)
	_, err = vault.Children(t.Context(), 1<<62, ChildrenOptions{})
	require.ErrorIs(err, ErrNotFound)
	_, err = vault.Children(t.Context(), dir.ID, ChildrenOptions{Limit: MaxChildrenLimit + 1})
	require.Error(err)
	_, err = vault.Children(t.Context(), dir.ID, ChildrenOptions{Offset: -1})
	require.Error(err)
}

func TestEmbeddedVersions(t *testing.T) {
	tests := []struct {
		name   string
		driver docsqlite.Driver
	}{
		{name: "build default"},
		{name: "pure Go", driver: modernc.Driver{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			testEmbeddedVersions(t, test.driver)
		})
	}
}

func testEmbeddedVersions(t *testing.T, driver docsqlite.Driver) {
	t.Helper()
	require := require.New(t)
	ctx := t.Context()
	vault, err := New(ctx, Config{Root: t.TempDir(), SQLite: driver})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(vault.Close()) })

	receipt, err := vault.Put(ctx, "/notes/entry.md", strings.NewReader("first\n"), PutOptions{})
	require.NoError(err)
	second, err := vault.Put(ctx, "/notes/entry.md", strings.NewReader("second\n"), PutOptions{})
	require.NoError(err)
	third, err := vault.Put(ctx, "/notes/entry.md", strings.NewReader("third\n"), PutOptions{})
	require.NoError(err)

	page, err := vault.Versions(ctx, receipt.Node.ID, VersionsOptions{Limit: 2})
	require.NoError(err)
	require.Equal(3, page.Total)
	require.Equal(2, page.Limit)
	require.Zero(page.Offset)
	require.Len(page.Items, 2)
	require.Equal([]string{third.Version.ID, second.Version.ID}, []string{
		page.Items[0].ID, page.Items[1].ID,
	})

	secondPage, err := vault.Versions(ctx, receipt.Node.ID, VersionsOptions{Limit: 2, Offset: 2})
	require.NoError(err)
	require.Equal(3, secondPage.Total)
	require.Equal(2, secondPage.Limit)
	require.Equal(2, secondPage.Offset)
	require.Len(secondPage.Items, 1)
	require.Equal([]string{receipt.Version.ID}, []string{secondPage.Items[0].ID})

	defaultPage, err := vault.Versions(ctx, receipt.Node.ID, VersionsOptions{})
	require.NoError(err)
	require.Equal(DefaultVersionsLimit, defaultPage.Limit)
	require.Len(defaultPage.Items, 3)

	directory, err := vault.Stat(ctx, "/notes")
	require.NoError(err)
	_, err = vault.Versions(ctx, directory.ID, VersionsOptions{})
	require.ErrorIs(err, ErrNotFile)
	_, err = vault.Versions(ctx, 1<<62, VersionsOptions{})
	require.ErrorIs(err, ErrNotFound)
	_, err = vault.Versions(ctx, receipt.Node.ID, VersionsOptions{Limit: MaxVersionsLimit + 1})
	require.Error(err)
	_, err = vault.Versions(ctx, receipt.Node.ID, VersionsOptions{Offset: -1})
	require.Error(err)

	require.NoError(vault.Close())
	_, err = vault.Versions(ctx, receipt.Node.ID, VersionsOptions{})
	require.ErrorIs(err, ErrClosed)
}

func TestEmbeddedVersionContent(t *testing.T) {
	tests := []struct {
		name   string
		driver docsqlite.Driver
	}{
		{name: "build default"},
		{name: "pure Go", driver: modernc.Driver{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			testEmbeddedVersionContent(t, test.driver)
		})
	}
}

func testEmbeddedVersionContent(t *testing.T, driver docsqlite.Driver) {
	t.Helper()
	require := require.New(t)
	ctx := t.Context()
	vault, err := New(ctx, Config{Root: t.TempDir(), SQLite: driver})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(vault.Close()) })

	first, err := vault.Put(ctx, "/notes/entry.md", strings.NewReader("first\n"), PutOptions{})
	require.NoError(err)
	_, err = vault.Put(ctx, "/notes/entry.md", strings.NewReader("second\n"), PutOptions{})
	require.NoError(err)

	content, err := vault.OpenVersionContent(ctx, first.Version.ID)
	require.NoError(err)
	got, err := io.ReadAll(content.Reader)
	require.NoError(err)
	require.NoError(content.Reader.Verify())
	require.NoError(content.Reader.Close())
	require.Equal([]byte("first\n"), got)
	require.Equal(first.Version.ID, content.Version.ID)

	_, err = vault.OpenVersionContent(ctx, "00000000-0000-4000-8000-000000000000")
	require.ErrorIs(err, ErrNotFound)

	require.NoError(vault.Close())
	_, err = vault.OpenVersionContent(ctx, first.Version.ID)
	require.ErrorIs(err, ErrClosed)
}

func TestEmbeddedVersionContentRejectsSizeMismatch(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	vault, err := New(t.Context(), Config{Root: root})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(vault.Close()) })

	first, err := vault.Put(
		t.Context(), "/notes/entry.md", strings.NewReader("first\n"), PutOptions{})
	require.NoError(err)
	blobPath := filepath.Join(root, "blobs", first.Version.BlobHash[:2], first.Version.BlobHash)
	require.NoError(os.WriteFile(blobPath, []byte("short"), 0o600))

	_, err = vault.OpenVersionContent(t.Context(), first.Version.ID)
	require.ErrorContains(err, "catalog size 5 does not match version size 6")
}

func TestEmbeddedVersionContentRejectsSameSizeCorruption(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	vault, err := New(t.Context(), Config{Root: root})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(vault.Close()) })

	first, err := vault.Put(
		t.Context(), "/notes/entry.md", strings.NewReader("first\n"), PutOptions{})
	require.NoError(err)
	blobPath := filepath.Join(root, "blobs", first.Version.BlobHash[:2], first.Version.BlobHash)
	corrupt := []byte("wrong\n")
	require.Len(corrupt, int(first.Version.Size))
	require.NoError(os.WriteFile(blobPath, corrupt, 0o600))

	content, err := vault.OpenVersionContent(t.Context(), first.Version.ID)
	require.NoError(err)
	got, err := io.ReadAll(content.Reader)
	require.ErrorIs(err, packstore.ErrContentMismatch)
	require.Equal(corrupt, got)
	require.ErrorIs(content.Reader.Verify(), packstore.ErrContentMismatch)
	require.Error(content.Reader.Close())
}

func TestPackBoundsWorkAndPreservesVerifiedContent(t *testing.T) {
	require := require.New(t)
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(vault.Close()) })

	contents := map[string]string{
		"/sessions/one.jsonl": strings.Repeat("first session line\n", 512),
		"/sessions/two.jsonl": strings.Repeat("second session line\n", 512),
	}
	for path, content := range contents {
		_, err := vault.Put(t.Context(), path, strings.NewReader(content), PutOptions{})
		require.NoError(err)
	}

	first, err := vault.Pack(t.Context(), PackOptions{MaxBytes: 1})
	require.NoError(err)
	require.Equal(1, first.PacksSealed)
	require.Equal(1, first.BlobsPacked)
	require.True(first.BudgetExhausted)

	second, err := vault.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	require.Equal(1, second.PacksSealed)
	require.Equal(1, second.BlobsPacked)
	require.False(second.BudgetExhausted)
	loose, err := vault.blobs.List()
	require.NoError(err)
	require.Empty(loose)

	idle, err := vault.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	require.Zero(idle.PacksSealed)
	require.Zero(idle.BlobsPacked)

	for path, want := range contents {
		content, err := vault.OpenContent(t.Context(), path)
		require.NoError(err)
		got, err := io.ReadAll(content.Reader)
		require.NoError(err)
		require.NoError(content.Reader.Verify())
		require.NoError(content.Reader.Close())
		require.Equal(want, string(got))
	}

	_, err = vault.Pack(t.Context(), PackOptions{MaxBytes: -1})
	require.Error(err)
}

func TestChildrenAndPackRejectClosedVault(t *testing.T) {
	require := require.New(t)
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(err)
	root, err := vault.Stat(t.Context(), "/")
	require.NoError(err)
	require.NoError(vault.Close())

	_, err = vault.Children(t.Context(), root.ID, ChildrenOptions{})
	require.ErrorIs(err, ErrClosed)
	_, err = vault.Pack(t.Context(), PackOptions{})
	require.ErrorIs(err, ErrClosed)
}

func nodeNames(nodes []Node) []string {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name)
	}
	return names
}

func TestEmbeddedVaultLifecycle(t *testing.T) {
	tests := []struct {
		name   string
		driver docsqlite.Driver
	}{
		{name: "build default"},
		{name: "pure Go", driver: modernc.Driver{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			testEmbeddedVaultLifecycle(t, test.driver)
		})
	}
}

func testEmbeddedVaultLifecycle(t *testing.T, driver docsqlite.Driver) {
	t.Helper()
	require := require.New(t)
	root := t.TempDir()
	vault, err := New(t.Context(), Config{Root: root, SQLite: driver})
	require.NoError(err)
	vaultID := vault.ID()
	require.NotEmpty(vaultID)

	first, err := vault.Put(t.Context(), "/sessions/one.jsonl", strings.NewReader("first\n"),
		PutOptions{MediaType: "application/x-ndjson"})
	require.NoError(err)
	require.True(first.Created)
	require.False(first.Replaced)
	require.Equal(int64(1), first.Version.NodeRevision)

	retry, err := vault.Put(t.Context(), "/sessions/one.jsonl", strings.NewReader("first\n"),
		PutOptions{MediaType: "application/x-ndjson", Expected: &first.Computed})
	require.NoError(err)
	require.False(retry.Created)
	require.False(retry.Replaced)
	require.Equal(first.Version.ID, retry.Version.ID)

	second, err := vault.Put(t.Context(), "/sessions/one.jsonl", strings.NewReader("second\n"),
		PutOptions{MediaType: "application/x-ndjson"})
	require.NoError(err)
	require.True(second.Replaced)
	require.Equal(first.Node.ID, second.Node.ID)
	require.Equal(int64(2), second.Version.NodeRevision)
	require.NotEqual(first.Version.ID, second.Version.ID)

	content, err := vault.OpenContent(t.Context(), "/sessions/one.jsonl")
	require.NoError(err)
	got, err := io.ReadAll(content.Reader)
	require.NoError(err)
	require.Equal("second\n", string(got))
	require.NoError(content.Reader.Close())
	require.NoError(vault.Close())

	reopened, err := New(t.Context(), Config{Root: root, SQLite: driver})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reopened.Close()) })
	require.Equal(vaultID, reopened.ID())
	node, err := reopened.Stat(t.Context(), "/sessions/one.jsonl")
	require.NoError(err)
	require.Equal(second.Node.ID, node.ID)
	require.Equal(second.Version.ID, node.CurrentVersionID)
}
