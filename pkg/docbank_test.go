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

func TestChildrenReturnsBoundedStablePages(t *testing.T) {
	require := require.New(t)
	vault, err := Open(t.Context(), OpenOptions{Root: t.TempDir()})
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

func TestPackBoundsWorkAndPreservesVerifiedContent(t *testing.T) {
	require := require.New(t)
	vault, err := Open(t.Context(), OpenOptions{Root: t.TempDir()})
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
	vault, err := Open(t.Context(), OpenOptions{Root: t.TempDir()})
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
	vault, err := Open(t.Context(), OpenOptions{Root: root, SQLite: driver})
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

	reopened, err := Open(t.Context(), OpenOptions{Root: root, SQLite: driver})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reopened.Close()) })
	require.Equal(vaultID, reopened.ID())
	node, err := reopened.Stat(t.Context(), "/sessions/one.jsonl")
	require.NoError(err)
	require.Equal(second.Node.ID, node.ID)
	require.Equal(second.Version.ID, node.CurrentVersionID)
}
