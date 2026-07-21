package docbank_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
