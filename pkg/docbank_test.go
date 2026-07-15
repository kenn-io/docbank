package docbank

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	docsqlite "go.kenn.io/docbank/pkg/sqlite"
	"go.kenn.io/docbank/pkg/sqlite/modernc"
)

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
			vault, err := Open(t.Context(), OpenOptions{Root: t.TempDir()})
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
	vault, err := Open(t.Context(), OpenOptions{Root: t.TempDir()})
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
	vault, err := Open(t.Context(), OpenOptions{Root: root, SQLite: driver})
	require.NoError(err)

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

	reopened, err := Open(t.Context(), OpenOptions{Root: root, SQLite: driver})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reopened.Close()) })
	node, err := reopened.Stat(t.Context(), "/sessions/one.jsonl")
	require.NoError(err)
	require.Equal(second.Node.ID, node.ID)
	require.Equal(second.Version.ID, node.CurrentVersionID)
}
