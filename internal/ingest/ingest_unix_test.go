//go:build unix

package ingest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportRefusesSymlinkAtOpen(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	src := writeTree(t, map[string]string{"real.txt": "hello"})
	link := filepath.Join(src, "link.txt")
	require.NoError(t, os.Symlink(filepath.Join(src, "real.txt"), link))

	// Classification via Lstat/WalkDir can be raced by a swap, so "symlinks
	// are skipped" has to hold at the point the file is opened for reading.
	ingestID, err := ing.Store.BeginIngest(ctx, "cli", "test")
	require.NoError(t, err)
	_, err = ing.importFile(ctx, ingestID, ing.Store.RootID(), link, link)
	require.Error(t, err)
}

func TestAddExplicitDirectorySymlinkPreservesSourceSpelling(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	target := writeTree(t, map[string]string{"nested/notes.txt": "hello"})
	alias := filepath.Join(t.TempDir(), "Dropbox")
	require.NoError(t, os.Symlink(target, alias))
	require.NoError(t, os.Symlink(
		filepath.Join(target, "nested", "notes.txt"),
		filepath.Join(target, "nested", "linked-notes.txt"),
	))

	rep, err := ing.AddPaths(ctx, []string{alias}, "/archive")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Added)
	assert.Zero(t, rep.Skipped)
	require.Len(t, rep.Failed, 1)
	assert.Equal(t, filepath.Join(alias, "nested", "linked-notes.txt"), rep.Failed[0].Path)

	node, err := ing.Store.NodeByPath(ctx, "/archive/Dropbox/nested/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(alias, "nested", "notes.txt"), provenancePath(t, ing, node.ID))

	// Re-running through the same user-facing spelling converges, and neither
	// the root link nor a nested link is replaced or followed as file content.
	rep, err = ing.AddPaths(ctx, []string{alias}, "/archive")
	require.NoError(t, err)
	assert.Zero(t, rep.Added)
	assert.Equal(t, 1, rep.Skipped)
	require.Len(t, rep.Failed, 1)
	rootInfo, err := os.Lstat(alias)
	require.NoError(t, err)
	assert.NotZero(t, rootInfo.Mode()&os.ModeSymlink)
	nestedInfo, err := os.Lstat(filepath.Join(target, "nested", "linked-notes.txt"))
	require.NoError(t, err)
	assert.NotZero(t, nestedInfo.Mode()&os.ModeSymlink)
}

func TestPreflightExplicitDirectorySymlinkUsesAliasAndSkipsNestedLinks(t *testing.T) {
	target := writeTree(t, map[string]string{"nested/session.jsonl": "{\"ok\":true}\n"})
	alias := filepath.Join(t.TempDir(), "Agent Sessions")
	require.NoError(t, os.Symlink(target, alias))
	nestedLink := filepath.Join(target, "nested", "latest.jsonl")
	require.NoError(t, os.Symlink(filepath.Join(target, "nested", "session.jsonl"), nestedLink))

	report, err := Preflight(t.Context(), []string{alias}, Options{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), report.Files)
	assert.Equal(t, int64(2), report.Directories)
	assert.Equal(t, int64(1), report.Skipped)
	require.Len(t, report.Findings, 1)
	assert.Equal(t, filepath.Join(alias, "nested", "latest.jsonl"), report.Findings[0].Path)
	assert.Equal(t, "skipped", report.Findings[0].Kind)
}

func provenancePath(t *testing.T, ing *Ingester, nodeID int64) string {
	t.Helper()
	var metadata bytes.Buffer
	require.NoError(t, ing.Store.ExportMetadata(t.Context(), &metadata))
	scanner := bufio.NewScanner(&metadata)
	for scanner.Scan() {
		var record struct {
			Type         string `json:"type"`
			NodeID       int64  `json:"node_id"`
			OriginalPath string `json:"original_path"`
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &record))
		if record.Type == "provenance" && record.NodeID == nodeID {
			return record.OriginalPath
		}
	}
	require.NoError(t, scanner.Err())
	t.Fatalf("no provenance for node %d", nodeID)
	return ""
}
