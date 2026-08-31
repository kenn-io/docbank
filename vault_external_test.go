package docbank_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	docbank "go.kenn.io/docbank"
)

func TestRootPackageConstructor(t *testing.T) {
	vault, err := docbank.New(context.Background(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
}

func createRangeFixture(
	t *testing.T, vault *docbank.Vault, virtualPath string, content []byte,
) docbank.PutReceipt {
	t.Helper()
	sum := sha256.Sum256(content)
	receipt, err := vault.Create(
		t.Context(), virtualPath, bytes.NewReader(content),
		docbank.CreateOptions{
			MediaType: "application/octet-stream",
			Expected: docbank.ContentIdentity{
				SHA256: hex.EncodeToString(sum[:]),
				Size:   int64(len(content)),
			},
		},
	)
	require.NoError(t, err)
	return receipt
}

func TestOpenVersionContentRangeRejectsInvalidSlices(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt := createRangeFixture(
		t, vault, "/ranges/value.bin", []byte("0123456789"),
	)

	cases := []docbank.ContentRangeOptions{
		{Offset: -1, Length: 1},
		{Offset: 0, Length: 0},
		{Offset: 0, Length: -1},
		{Offset: 10, Length: 1},
		{Offset: 9, Length: 2},
		{Offset: math.MaxInt64, Length: math.MaxInt64},
	}
	for _, opts := range cases {
		_, err := vault.OpenVersionContentRange(t.Context(), receipt.Version.ID, opts)
		require.ErrorIs(t, err, docbank.ErrInvalidContentRange)
	}
}

func TestOpenVersionContentRangeMissingVersion(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	_, err = vault.OpenVersionContentRange(
		t.Context(), "00000000-0000-4000-8000-000000000000",
		docbank.ContentRangeOptions{Offset: 0, Length: 1},
	)
	require.ErrorIs(t, err, docbank.ErrNotFound)
}

func TestOpenVersionContentRangeRawLoose(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt := createRangeFixture(
		t, vault, "/ranges/raw.bin", []byte("0123456789"),
	)

	got, err := vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 2, Length: 4},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, got.Reader.Close()) })
	body, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.Equal(t, []byte("2345"), body)
	require.Equal(t, receipt.Version, got.Version)
	require.Equal(t, int64(2), got.Offset)
	require.Equal(t, int64(4), got.Length)
}

func TestOpenVersionContentRangeHistoricalVersion(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	path := "/ranges/history.bin"
	first := createRangeFixture(t, vault, path, []byte("abcdefghij"))
	_, err = vault.Put(
		t.Context(), path, bytes.NewReader([]byte("ABCDEFGHIJ")),
		docbank.PutOptions{MediaType: "application/octet-stream"},
	)
	require.NoError(t, err)

	got, err := vault.OpenVersionContentRange(
		t.Context(), first.Version.ID,
		docbank.ContentRangeOptions{Offset: 1, Length: 3},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, got.Reader.Close()) })
	body, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.Equal(t, []byte("bcd"), body)
	require.Equal(t, first.Version.ID, got.Version.ID)
}

func TestOpenVersionContentRangeCompressedLoose(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{
		Root: t.TempDir(),
		LooseCompression: docbank.LooseCompressionOptions{
			Enabled: true, MinBytes: 1, MinSavingsPercent: 0,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	content := []byte(strings.Repeat("compressed logical range\n", 128))
	receipt := createRangeFixture(t, vault, "/ranges/compressed.bin", content)
	require.Equal(t, "loose", receipt.Physical.Kind)
	require.Equal(t, "zstd", receipt.Physical.Encoding)

	got, err := vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 11, Length: 17},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, got.Reader.Close()) })
	body, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.Equal(t, content[11:28], body)
}

func TestOpenVersionContentRangePacked(t *testing.T) {
	root := t.TempDir()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	content := []byte("packed logical range content")
	receipt := createRangeFixture(t, vault, "/ranges/packed.bin", content)
	report, err := vault.Pack(t.Context(), docbank.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, report.BlobsPacked)
	require.NoFileExists(t, looseBlobPath(root, receipt.Computed.SHA256))

	got, err := vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 7, Length: 7},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, got.Reader.Close()) })
	body, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.Equal(t, content[7:14], body)
}

func TestOpenVersionContentRangeUnavailable(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, string)
	}{
		{
			name: "missing authority",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Remove(path))
			},
		},
		{
			name: "physical size mismatch",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("short"), 0o600))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			content := []byte("physical authority bytes")
			receipt := createRangeFixture(t, vault, "/ranges/unavailable.bin", content)
			test.corrupt(t, looseBlobPath(root, receipt.Computed.SHA256))

			_, err = vault.OpenVersionContentRange(
				t.Context(), receipt.Version.ID,
				docbank.ContentRangeOptions{Offset: 0, Length: 1},
			)
			require.ErrorIs(t, err, docbank.ErrContentUnavailable)
		})
	}
}

func TestOpenVersionContentRangeHoldsVaultLease(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt := createRangeFixture(t, vault, "/ranges/lease.bin", []byte("lease bytes"))
	opened, err := vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 0, Length: 1},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, opened.Reader.Close()) })

	closeDone := make(chan error, 1)
	go func() { closeDone <- vault.Close() }()
	select {
	case err := <-closeDone:
		require.FailNow(t, "vault closed while a range held its lifecycle lease", "error: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, opened.Reader.Close())
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "vault did not close after the range released its lease")
	}
}

func TestOpenVersionContentRangeClosedVault(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	receipt := createRangeFixture(t, vault, "/ranges/closed.bin", []byte("closed bytes"))
	require.NoError(t, vault.Close())

	_, err = vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 0, Length: 1},
	)
	require.ErrorIs(t, err, docbank.ErrClosed)
}

func TestEmbeddedImmutableCreate(t *testing.T) {
	content := []byte("immutable external content\n")
	sum := sha256.Sum256(content)
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	receipt, err := vault.Create(t.Context(), "/external.txt", bytes.NewReader(content), docbank.CreateOptions{
		MediaType: "text/plain",
		Expected:  docbank.ContentIdentity{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))},
	})
	require.NoError(t, err)
	require.True(t, receipt.Created)
}

func TestEmbeddedPutRequiresCurrentRevision(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	created, err := vault.Put(
		t.Context(), "/external.txt", strings.NewReader("first\n"),
		docbank.PutOptions{MediaType: "text/plain"},
	)
	require.NoError(t, err)

	unchanged, err := vault.Put(
		t.Context(), "/external.txt", strings.NewReader("first\n"),
		docbank.PutOptions{MediaType: "text/plain", IfRevision: created.Node.Revision},
	)
	require.NoError(t, err)
	require.Equal(t, created.Version.ID, unchanged.Version.ID)
	require.Equal(t, created.Node.Revision, unchanged.Node.Revision)

	replaced, err := vault.Put(
		t.Context(), "/external.txt", strings.NewReader("second\n"),
		docbank.PutOptions{MediaType: "text/plain", IfRevision: created.Node.Revision},
	)
	require.NoError(t, err)
	require.NotEqual(t, created.Version.ID, replaced.Version.ID)

	_, err = vault.Put(
		t.Context(), "/external.txt", strings.NewReader("third\n"),
		docbank.PutOptions{MediaType: "text/plain", IfRevision: created.Node.Revision},
	)
	require.ErrorIs(t, err, docbank.ErrStaleRevision)

	current, err := vault.Stat(t.Context(), "/external.txt")
	require.NoError(t, err)
	require.Equal(t, replaced.Version.ID, current.CurrentVersionID)

	_, err = vault.Put(
		t.Context(), "/external.txt", strings.NewReader("invalid\n"),
		docbank.PutOptions{MediaType: "text/plain", IfRevision: -1},
	)
	require.Error(t, err)
}

func TestVaultMoveTrashRestoreExternalAPI(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	created, err := vault.Put(
		t.Context(), "/inbox/report.txt", strings.NewReader("report\n"), docbank.PutOptions{},
	)
	require.NoError(t, err)

	moved, err := vault.MovePath(t.Context(), "/inbox/report.txt", "/archive.txt", docbank.RevisionOptions{
		IfRevision: created.Node.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, created.Node.ID, moved.Node.ID)
	require.Equal(t, created.Node.Revision+1, moved.Node.Revision)
	require.Equal(t, "/archive.txt", moved.Path)

	trashed, err := vault.TrashPath(t.Context(), moved.Path, docbank.RevisionOptions{
		IfRevision: moved.Node.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, moved.Path, trashed.Path)
	restored, err := vault.Restore(t.Context(), trashed.Node.ID, docbank.RevisionOptions{
		IfRevision: trashed.Node.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, moved.Path, restored.Path)

	_, err = vault.TrashPath(t.Context(), restored.Path, docbank.RevisionOptions{})
	require.NoError(t, err)
	report, err := vault.EmptyTrash(t.Context(), docbank.TrashEmptyOptions{MaxRoots: 1, DryRun: true})
	require.NoError(t, err)
	require.Equal(t, int64(1), report.Candidates)
	require.True(t, report.DryRun)
}

func TestVaultMoveBatchExternalAPI(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	first, err := vault.Put(t.Context(), "/left/first.txt", strings.NewReader("first\n"), docbank.PutOptions{})
	require.NoError(t, err)
	second, err := vault.Put(t.Context(), "/right/second.txt", strings.NewReader("second\n"), docbank.PutOptions{})
	require.NoError(t, err)

	receipts, err := vault.BatchMove(t.Context(), []docbank.BatchMoveItem{
		{SourcePath: "/left/first.txt", DestinationPath: "/right/second.txt"},
		{NodeID: second.Node.ID, IfRevision: second.Node.Revision, DestinationPath: "/left/first.txt"},
	})
	require.NoError(t, err)
	require.Len(t, receipts, 2)
	require.Equal(t, first.Node.ID, receipts[0].Node.ID)
	require.Equal(t, "/left/first.txt", receipts[0].FromPath)
	require.Equal(t, "/right/second.txt", receipts[0].Path)
	require.Equal(t, second.Node.ID, receipts[1].Node.ID)
	require.Equal(t, "/left/first.txt", receipts[1].Path)
}

func TestTreeMutationErrorsAreClassifiableOutsidePackage(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	created, err := vault.Put(
		t.Context(), "/parent/child/document.txt", strings.NewReader("document\n"),
		docbank.PutOptions{},
	)
	require.NoError(t, err)

	_, err = vault.Restore(t.Context(), created.Node.ID, docbank.RevisionOptions{})
	require.ErrorIs(t, err, docbank.ErrNotTrashed)
	_, err = vault.TrashPath(t.Context(), "/", docbank.RevisionOptions{})
	require.ErrorIs(t, err, docbank.ErrIsRoot)
	_, err = vault.MovePath(
		t.Context(), "/parent/child/document.txt", "/parent/../document.txt",
		docbank.RevisionOptions{},
	)
	require.ErrorIs(t, err, docbank.ErrInvalidName)
	_, err = vault.MovePath(
		t.Context(), "/parent", "/parent/child/parent", docbank.RevisionOptions{},
	)
	require.ErrorIs(t, err, docbank.ErrCycle)

	// Existing audited vaults can surface this sentinel through the same public
	// methods even though first enrollment is currently daemon-owned.
	require.ErrorIs(t, fmt.Errorf("embedded audited mutation: %w", docbank.ErrAuditMutationUnsupported), docbank.ErrAuditMutationUnsupported)
}

func TestOpenContentClassifiesPhysicalContentFailures(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, string)
	}{
		{
			name: "missing blob",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Remove(path))
			},
		},
		{
			name: "physical size mismatch",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("short"), 0o600))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })

			receipt, err := vault.Put(
				t.Context(), "/notes/current.md", strings.NewReader("current bytes\n"), docbank.PutOptions{},
			)
			require.NoError(t, err)
			test.corrupt(t, looseBlobPath(root, receipt.Computed.SHA256))

			_, err = vault.OpenContent(t.Context(), "/notes/current.md")
			require.ErrorIs(t, err, docbank.ErrContentUnavailable)
		})
	}
}

func TestOpenVersionContentClassifiesPhysicalContentFailures(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, string)
	}{
		{
			name: "missing blob",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Remove(path))
			},
		},
		{
			name: "physical size mismatch",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("short"), 0o600))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })

			first, err := vault.Put(
				t.Context(), "/notes/history.md", strings.NewReader("historical bytes\n"), docbank.PutOptions{},
			)
			require.NoError(t, err)
			_, err = vault.Put(
				t.Context(), "/notes/history.md", strings.NewReader("current bytes\n"), docbank.PutOptions{},
			)
			require.NoError(t, err)
			test.corrupt(t, looseBlobPath(root, first.Computed.SHA256))

			_, err = vault.OpenVersionContent(t.Context(), first.Version.ID)
			require.ErrorIs(t, err, docbank.ErrContentUnavailable)
		})
	}
}

func TestContentMetadataErrorsRemainDistinctFromPhysicalUnavailability(t *testing.T) {
	root := t.TempDir()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)

	receipt, err := vault.Put(
		t.Context(), "/notes/entry.md", strings.NewReader("entry\n"), docbank.PutOptions{},
	)
	require.NoError(t, err)

	_, err = vault.OpenContent(t.Context(), "/missing.md")
	require.ErrorIs(t, err, docbank.ErrNotFound)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)

	_, err = vault.OpenContent(t.Context(), "/notes")
	require.ErrorIs(t, err, docbank.ErrNotFile)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)

	_, err = vault.OpenVersionContent(t.Context(), "00000000-0000-4000-8000-000000000000")
	require.ErrorIs(t, err, docbank.ErrNotFound)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)

	require.NoError(t, vault.Close())

	_, err = vault.OpenContent(t.Context(), "/notes/entry.md")
	require.ErrorIs(t, err, docbank.ErrClosed)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)

	_, err = vault.OpenVersionContent(t.Context(), receipt.Version.ID)
	require.ErrorIs(t, err, docbank.ErrClosed)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)
}

func looseBlobPath(root, hash string) string {
	return filepath.Join(root, "blobs", hash[:2], hash)
}
