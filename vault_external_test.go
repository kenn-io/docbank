package docbank_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	docbank "go.kenn.io/docbank"
)

func TestRootPackageConstructor(t *testing.T) {
	vault, err := docbank.New(context.Background(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
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
