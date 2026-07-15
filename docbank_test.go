package docbank

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/modernc"
)

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
