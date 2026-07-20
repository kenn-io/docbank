package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

// runCLI executes the root command against a test vault (caller must have
// set DOCBANK_HOME via t.Setenv) and returns captured stdout.
//
// It must not pass t.Context(): cobra caches the first context on each
// subcommand (Command.ctx is only assigned when nil), so a later test would
// execute against an earlier test's canceled context.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	resetFlags(rootCmd)
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	err := rootCmd.ExecuteContext(context.Background())
	rootCmd.SetArgs(nil)
	return out.String(), err
}

// resetFlags restores every command's flags to their defaults. Package-level
// flag vars persist across in-process Execute calls, and a parse or argument
// validation failure exits before any command code could reset them, so the
// wrapper is the only reliable place.
func resetFlags(c *cobra.Command) {
	c.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Changed {
			if values, ok := f.Value.(pflag.SliceValue); ok && f.DefValue == "[]" {
				_ = values.Replace(nil)
			} else {
				_ = f.Value.Set(f.DefValue)
			}
			f.Changed = false
		}
	})
	for _, sub := range c.Commands() {
		resetFlags(sub)
	}
}

func setupVaultHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)
	startTestDaemon(t, dir)
	return dir
}

// startTestDaemon runs runServe in-process against dir and tears it down
// with the test. CLI commands discover it through the runtime record like
// production; no test-only transport exists.
func startTestDaemon(t *testing.T, dir string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("test daemon did not shut down")
		}
	})
	require.Eventually(t, func() bool {
		_, _, ok, err := client.Find(ctx, dir)
		return err == nil && ok
	}, 10*time.Second, 25*time.Millisecond, "test daemon never became ready")
}

func writeSourceFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

func TestVersionReportsInstalledProgram(t *testing.T) {
	out, err := runCLI(t, "version")
	require.NoError(t, err)
	assert.Equal(t, "docbank version dev (unknown)\n", out)

	_, err = runCLI(t, "--version")
	require.ErrorContains(t, err, "unknown flag: --version")

	_, err = runCLI(t, "version", "94ddbb2a-bf69-43d7-9236-c2ab9d27a79a")
	require.ErrorContains(t, err, "unknown command")

	_, err = runCLI(t, "versions", "/old-implicit-list")
	require.ErrorContains(t, err, "unknown command")
}

func TestAddLsTreeCat(t *testing.T) {
	_ = setupVaultHome(t)
	src := writeSourceFile(t, "notes.txt", "hello vault")

	out, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)
	assert.Contains(t, out, "added: 1")

	out, err = runCLI(t, "ls", "/inbox")
	require.NoError(t, err)
	assert.Contains(t, out, "notes.txt")
	c, err := client.Ensure(context.Background())
	require.NoError(t, err)
	inbox, err := c.Stat(context.Background(), "/inbox")
	require.NoError(t, err)
	note, err := c.Stat(context.Background(), "/inbox/notes.txt")
	require.NoError(t, err)
	out, err = runCLI(t, "ls", formatNodeSelector(inbox.ID))
	require.NoError(t, err)
	assert.Contains(t, out, formatNodeSelector(note.ID))

	out, err = runCLI(t, "tree", formatNodeSelector(1))
	require.NoError(t, err)
	assert.Contains(t, out, "inbox")
	assert.Contains(t, out, "notes.txt")
	assert.Contains(t, out, "["+formatNodeSelector(note.ID)+"]")

	out, err = runCLI(t, "cat", formatNodeSelector(note.ID))
	require.NoError(t, err)
	assert.Equal(t, "hello vault", out)

	out, err = runCLI(t, "versions", "list", "/inbox/notes.txt")
	require.NoError(t, err)
	assert.Contains(t, out, "content_create")
	versionID := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`).
		FindString(out)
	require.NotEmpty(t, versionID)

	out, err = runCLI(t, "versions", "show", versionID, "--json")
	require.NoError(t, err)
	var version api.ContentVersion
	require.NoError(t, json.Unmarshal([]byte(out), &version))
	assert.Equal(t, versionID, version.ID)
	assert.Equal(t, "content_create", version.TransitionKind)

	out, err = runCLI(t, "versions", "cat", versionID)
	require.NoError(t, err)
	assert.Equal(t, "hello vault", out)
}

func TestTreeBoundsAndReportsOmissions(t *testing.T) {
	_ = setupVaultHome(t)
	c, err := client.Ensure(context.Background())
	require.NoError(t, err)
	root, err := c.Stat(context.Background(), "/")
	require.NoError(t, err)

	parent := root
	for _, name := range []string{"one", "two", "three", "four", "five"} {
		parent, err = c.Mkdir(context.Background(), parent.ID, name)
		require.NoError(t, err)
	}

	out, err := runCLI(t, "tree", "/", "--json")
	require.NoError(t, err)
	var listing treeListing
	require.NoError(t, json.Unmarshal([]byte(out), &listing))
	require.Len(t, listing.Items, defaultTreeDepth)
	assert.True(t, listing.Truncated)
	assert.Equal(t, []treeOmission{{
		Path: "/one/two/three/four", Reason: "depth_limit", DirectChildren: 1,
	}}, listing.Omissions)

	out, err = runCLI(t, "tree", "/", "--json", "--depth", "2")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &listing))
	require.Len(t, listing.Items, 2)
	assert.Equal(t, "/one/two", listing.Items[1].Path)
	assert.Equal(t, []treeOmission{{
		Path: "/one/two", Reason: "depth_limit", DirectChildren: 1,
	}}, listing.Omissions)

	out, err = runCLI(t, "tree", "/", "--json", "--max-entries", "2")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &listing))
	require.Len(t, listing.Items, 2)
	assert.Equal(t, []treeOmission{{
		Path: "/one/two", Reason: "entry_limit", DirectChildren: 1,
	}}, listing.Omissions)

	out, err = runCLI(t, "tree", "/", "--all", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &listing))
	require.Len(t, listing.Items, 5)
	assert.False(t, listing.Truncated)
	assert.Empty(t, listing.Omissions)
	assert.Contains(t, out, `"omissions":[]`)

	out, err = runCLI(t, "tree", "/", "--depth", "2")
	require.NoError(t, err)
	assert.Contains(t, out, "... tree truncated at 1 boundary(s):")
	assert.Contains(t, out, "/one/two: 1 direct entry hidden by depth limit")
	assert.Contains(t, out, "use --all deliberately")

	for _, args := range [][]string{
		{"tree", "--depth", "0"},
		{"tree", "--max-entries", "0"},
		{"tree", "--all", "--depth", "2"},
	} {
		_, err = runCLI(t, args...)
		require.Error(t, err)
		var classified *exitError
		require.ErrorAs(t, err, &classified)
		assert.Equal(t, exitUsage, classified.code)
	}
}

func TestLegacyReadCommandsJSON(t *testing.T) {
	_ = setupVaultHome(t)

	out, err := runCLI(t, "ls", "/", "--json")
	require.NoError(t, err)
	var listed directoryListing
	require.NoError(t, json.Unmarshal([]byte(out), &listed))
	assert.Equal(t, "/", listed.Directory.Path)
	assert.Empty(t, listed.Items)
	assert.Contains(t, out, `"items":[]`)

	out, err = runCLI(t, "tree", "/", "--json")
	require.NoError(t, err)
	var tree treeListing
	require.NoError(t, json.Unmarshal([]byte(out), &tree))
	assert.Equal(t, "/", tree.Root.Path)
	assert.Empty(t, tree.Items)
	assert.Contains(t, out, `"items":[]`)
	assert.False(t, tree.Truncated)
	assert.Contains(t, out, `"omissions":[]`)

	src := writeSourceFile(t, "notes.txt", "searchable notes")
	_, err = runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	out, err = runCLI(t, "ls", "/inbox", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &listed))
	require.Len(t, listed.Items, 1)
	assert.Equal(t, "notes.txt", listed.Items[0].Name)

	out, err = runCLI(t, "tree", "/", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &tree))
	require.Len(t, tree.Items, 2)
	assert.Equal(t, "/inbox", tree.Items[0].Path)
	assert.Equal(t, 1, tree.Items[0].Depth)
	assert.Equal(t, "/inbox/notes.txt", tree.Items[1].Path)
	assert.Equal(t, 2, tree.Items[1].Depth)

	out, err = runCLI(t, "search", "notes", "--json")
	require.NoError(t, err)
	var search api.SearchReport
	require.NoError(t, json.Unmarshal([]byte(out), &search))
	require.Len(t, search.Hits, 1)
	assert.Equal(t, "/inbox/notes.txt", search.Hits[0].Path)

	_, err = runCLI(t, "rm", "/inbox/notes.txt")
	require.NoError(t, err)
	out, err = runCLI(t, "trash", "list", "--json")
	require.NoError(t, err)
	var trash trashListing
	require.NoError(t, json.Unmarshal([]byte(out), &trash))
	require.Len(t, trash.Items, 1)
	assert.Equal(t, "notes.txt", trash.Items[0].Name)

	out, err = runCLI(t, "trash", "empty", "--json")
	require.NoError(t, err)
	var empty api.TrashEmptyReport
	require.NoError(t, json.Unmarshal([]byte(out), &empty))
	assert.EqualValues(t, 1, empty.CandidateRoots)
	assert.False(t, empty.Run)
	assert.NotContains(t, out, "dry run")
}

func TestAuditEnableIsPreviewFirstAndReportsProtection(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "return.txt", "tax return")
	_, err := runCLI(t, "add", source, "--dest", "/Taxes")
	require.NoError(t, err)
	c, err := client.Ensure(context.Background())
	require.NoError(t, err)
	taxes, err := c.Stat(context.Background(), "/Taxes")
	require.NoError(t, err)
	returnNode, err := c.Stat(context.Background(), "/Taxes/return.txt")
	require.NoError(t, err)

	out, err := runCLI(t, "audit", "status", "--json")
	require.NoError(t, err, out)
	var dormant api.AuditStatus
	require.NoError(t, json.Unmarshal([]byte(out), &dormant))
	assert.False(t, dormant.Enabled)

	out, err = runCLI(t, "audit", "enable", formatNodeSelector(taxes.ID), "--json")
	require.NoError(t, err, out)
	var preview api.AuditEnrollmentPreview
	require.NoError(t, json.Unmarshal([]byte(out), &preview))
	assert.Equal(t, "/Taxes", preview.TargetPath)
	assert.Equal(t, 2, preview.MemberCount)
	assert.NotEmpty(t, preview.PreviewToken)
	humanPreview, err := runCLI(t, "audit", "enable", "/Taxes")
	require.NoError(t, err, humanPreview)
	assert.Contains(t, humanPreview, "Vault-wide permanent metadata:")
	assert.Contains(t, humanPreview, "including outside the selected scope")

	_, err = runCLI(t, "audit", "enable", "--run", "--token", preview.PreviewToken)
	require.ErrorContains(t, err, "--acknowledge-permanent-retention")

	out, err = runCLI(t, "audit", "enable", "--run", "--token", preview.PreviewToken,
		"--acknowledge-permanent-retention", "--json")
	require.NoError(t, err, out)
	var status api.AuditStatus
	require.NoError(t, json.Unmarshal([]byte(out), &status))
	assert.True(t, status.Enabled)
	require.Len(t, status.Scopes, 1)
	assert.Equal(t, preview.ScopeID, status.Scopes[0].ID)

	out, err = runCLI(t, "audit", "status", formatNodeSelector(returnNode.ID))
	require.NoError(t, err, out)
	assert.Contains(t, out, `"/Taxes/return.txt" is permanently protected`)

	replacement := writeSourceFile(t, "replacement.txt", "amended return")
	_, err = runCLI(t, "put", replacement, "/Taxes/return.txt", "--progress", "plain")
	require.NoError(t, err)
	out, err = runCLI(t, "audit", "history", formatNodeSelector(returnNode.ID), "--json")
	require.NoError(t, err, out)
	var history api.AuditEventPage
	require.NoError(t, json.Unmarshal([]byte(out), &history))
	require.NotEmpty(t, history.Items)
	assert.Equal(t, "content_replace", history.Items[0].Kind)
	assert.Equal(t, "/Taxes/return.txt", history.Path)
	humanHistory, err := runCLI(t, "audit", "history", "/Taxes/return.txt")
	require.NoError(t, err, humanHistory)
	assert.Contains(t, humanHistory, "content_replace")
	assert.Contains(t, humanHistory, `audit history for "/Taxes/return.txt"`)

	_, err = runCLI(t, "audit", "enable", "--run", "--token", preview.PreviewToken,
		"--acknowledge-permanent-retention")
	require.ErrorIs(t, err, store.ErrAuditPreviewStale)
}

func TestPutReplacesContentAndRetainsHistory(t *testing.T) {
	_ = setupVaultHome(t)
	initial := writeSourceFile(t, "document.txt", "initial content")
	_, err := runCLI(t, "add", initial, "--dest", "/inbox")
	require.NoError(t, err)
	c, err := client.Ensure(context.Background())
	require.NoError(t, err)
	document, err := c.Stat(context.Background(), "/inbox/document.txt")
	require.NoError(t, err)
	documentSelector := formatNodeSelector(document.ID)

	out, err := runCLI(t, "versions", "list", documentSelector, "--json")
	require.NoError(t, err)
	var initialPage api.ContentVersionPage
	require.NoError(t, json.Unmarshal([]byte(out), &initialPage))
	require.Len(t, initialPage.Items, 1)
	initialVersion := initialPage.Items[0].ID

	replacement := writeSourceFile(t, "replacement.bin", "replacement content")
	out, err = runCLI(t, "put", replacement, documentSelector,
		"--mime-type", "text/plain", "--progress", "plain")
	require.NoError(t, err, out)
	assert.Contains(t, out, "hash:")
	assert.Contains(t, out, "upload:")
	assert.Contains(t, out, "updated /inbox/document.txt to version")

	out, err = runCLI(t, "cat", "/inbox/document.txt")
	require.NoError(t, err)
	assert.Equal(t, "replacement content", out)
	out, err = runCLI(t, "versions", "cat", initialVersion)
	require.NoError(t, err)
	assert.Equal(t, "initial content", out, "put must retain the prior immutable bytes")
	out, err = runCLI(t, "versions", "list", "/inbox/document.txt")
	require.NoError(t, err)
	assert.Contains(t, out, "content_replace")
	assert.Contains(t, out, "content_create")

	again := writeSourceFile(t, "again.bin", "third version")
	out, err = runCLI(t, "put", again, "/inbox/document.txt",
		"--mime-type", "application/octet-stream", "--json")
	require.NoError(t, err, out)
	assert.NotContains(t, out, "hash:", "JSON output must not contain progress lines")
	assert.NotContains(t, out, "upload:", "JSON output must not contain progress lines")
	var receipt api.ContentReplacementReceipt
	require.NoError(t, json.Unmarshal([]byte(out), &receipt))
	assert.Equal(t, "content_replace", receipt.Version.TransitionKind)
	assert.Equal(t, int64(3), receipt.Node.Revision)
	assert.Equal(t, "application/octet-stream", receipt.Node.MimeType)

	out, err = runCLI(t, "revert", documentSelector, initialVersion)
	require.NoError(t, err, out)
	assert.Contains(t, out, "reverted /inbox/document.txt to source version "+initialVersion)
	out, err = runCLI(t, "cat", "/inbox/document.txt")
	require.NoError(t, err)
	assert.Equal(t, "initial content", out)

	// Selecting the same historical source again records another deliberate
	// revert rather than mutating or reusing the current history row.
	out, err = runCLI(t, "revert", "/inbox/document.txt", initialVersion, "--json")
	require.NoError(t, err, out)
	var reverted api.ContentReversionReceipt
	require.NoError(t, json.Unmarshal([]byte(out), &reverted))
	assert.Equal(t, initialVersion, reverted.SourceVersion.ID)
	assert.Equal(t, "content_revert", reverted.Version.TransitionKind)
	assert.Equal(t, int64(5), reverted.Node.Revision)
	out, err = runCLI(t, "versions", "show", reverted.Version.ID)
	require.NoError(t, err)
	assert.Contains(t, out, "Source version:  "+initialVersion)
}

func TestVersionsPruneIsPreviewFirstAndKeepsCurrentContent(t *testing.T) {
	_ = setupVaultHome(t)
	initial := writeSourceFile(t, "document.txt", "initial content")
	_, err := runCLI(t, "add", initial, "--dest", "/inbox")
	require.NoError(t, err)

	second := writeSourceFile(t, "second.txt", "second content")
	_, err = runCLI(t, "put", second, "/inbox/document.txt", "--progress", "plain")
	require.NoError(t, err)
	third := writeSourceFile(t, "third.txt", "current content")
	_, err = runCLI(t, "put", third, "/inbox/document.txt", "--progress", "plain")
	require.NoError(t, err)

	out, err := runCLI(t, "versions", "list", "/inbox/document.txt", "--json")
	require.NoError(t, err)
	var before api.ContentVersionPage
	require.NoError(t, json.Unmarshal([]byte(out), &before))
	require.Len(t, before.Items, 3)
	c, err := client.Ensure(context.Background())
	require.NoError(t, err)
	document, err := c.Stat(context.Background(), "/inbox/document.txt")
	require.NoError(t, err)
	documentSelector := formatNodeSelector(document.ID)
	_, err = runCLI(t, "versions", "prune", "/inbox/document.txt",
		"--version", before.Items[1].ID+","+before.Items[2].ID)
	require.ErrorContains(t, err, "canonical UUIDv4",
		"repeatable --version values must treat commas literally")
	_, err = runCLI(t, "versions", "prune", "/inbox/document.txt", "--keep-newest", "0")
	require.ErrorContains(t, err, "--keep-newest must be at least 1")

	out, err = runCLI(t, "versions", "prune", "/inbox/document.txt", "--keep-newest", "1")
	require.NoError(t, err, out)
	assert.Contains(t, out, "2 version(s) selected")
	assert.Contains(t, out, "dry run")
	assert.Contains(t, out, "pending gc")
	out, err = runCLI(t, "versions", "list", "/inbox/document.txt", "--json")
	require.NoError(t, err)
	var afterPreview api.ContentVersionPage
	require.NoError(t, json.Unmarshal([]byte(out), &afterPreview))
	assert.Equal(t, 3, afterPreview.Total, "preview must retain history")

	out, err = runCLI(t, "versions", "prune", documentSelector,
		"--keep-newest", "1", "--run")
	require.NoError(t, err, out)
	assert.Contains(t, out, "pruned 2 version(s)")
	out, err = runCLI(t, "versions", "list", "/inbox/document.txt", "--json")
	require.NoError(t, err)
	var afterRun api.ContentVersionPage
	require.NoError(t, json.Unmarshal([]byte(out), &afterRun))
	require.Len(t, afterRun.Items, 1)
	assert.Equal(t, before.Items[0].ID, afterRun.Items[0].ID)
	out, err = runCLI(t, "versions", "prune", "/inbox/document.txt", "--all-prior", "--run")
	require.NoError(t, err, out)
	assert.Contains(t, out, "pruned 0 version(s); nothing to do")
	out, err = runCLI(t, "cat", "/inbox/document.txt")
	require.NoError(t, err)
	assert.Equal(t, "current content", out)
}

func TestTagCLIOrganizesNodesByNameOrStableID(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "return.pdf", "tax return")
	_, err := runCLI(t, "add", source, "--dest", "/records")
	require.NoError(t, err)
	c, err := client.Ensure(context.Background())
	require.NoError(t, err)
	returnNode, err := c.Stat(context.Background(), "/records/return.pdf")
	require.NoError(t, err)

	out, err := runCLI(t, "tag", "list")
	require.NoError(t, err)
	assert.Equal(t, "no tags\n", out)

	out, err = runCLI(t, "tag", "create", "taxes", "--json")
	require.NoError(t, err, out)
	var tag api.Tag
	require.NoError(t, json.Unmarshal([]byte(out), &tag))
	assert.Equal(t, "taxes", tag.Name)
	assert.NotEmpty(t, tag.ID)
	out, err = runCLI(t, "tag", "list", "--offset", "100")
	require.NoError(t, err, out)
	assert.Equal(t, "no tags at offset 100 (1 total)\n", out)

	out, err = runCLI(t, "tag", "assign", "taxes", "/records/return.pdf")
	require.NoError(t, err, out)
	assert.Contains(t, out, `assigned tag "taxes" to /records/return.pdf`)
	out, err = runCLI(t, "tag", "assign", tag.ID, formatNodeSelector(returnNode.ID))
	require.NoError(t, err, out)
	assert.Contains(t, out, `already assigned tag "taxes"`)
	out, err = runCLI(t, "tag", "nodes", "taxes", "--offset", "100")
	require.NoError(t, err, out)
	assert.Equal(t, "no nodes for tag \"taxes\" at offset 100 (1 total)\n", out)

	out, err = runCLI(t, "tag", "list")
	require.NoError(t, err, out)
	assert.Contains(t, out, "ASSIGNMENTS")
	assert.Contains(t, out, "taxes")
	out, err = runCLI(t, "tag", "show", tag.ID, "--json")
	require.NoError(t, err, out)
	var shown api.Tag
	require.NoError(t, json.Unmarshal([]byte(out), &shown))
	assert.Equal(t, tag.ID, shown.ID)
	assert.Equal(t, 1, shown.AssignmentCount)

	out, err = runCLI(t, "tag", "nodes", "taxes", "--json")
	require.NoError(t, err, out)
	var nodes api.TaggedNodePage
	require.NoError(t, json.Unmarshal([]byte(out), &nodes))
	require.Len(t, nodes.Items, 1)
	assert.Equal(t, "/records/return.pdf", nodes.Items[0].Path)

	out, err = runCLI(t, "tag", "rename", "taxes", "tax archive")
	require.NoError(t, err, out)
	assert.Contains(t, out, `renamed tag "taxes" to "tax archive"`)
	out, err = runCLI(t, "tag", "delete", "tax archive", "--json")
	require.NoError(t, err, out)
	var deleted api.TagDeletionReceipt
	require.NoError(t, json.Unmarshal([]byte(out), &deleted))
	assert.Equal(t, 1, deleted.RemovedAssignments)

	out, err = runCLI(t, "tag", "list", "--json")
	require.NoError(t, err, out)
	var page api.TagPage
	require.NoError(t, json.Unmarshal([]byte(out), &page))
	assert.Zero(t, page.Total)
	assert.Empty(t, page.Items)

	// A UUID-shaped display name must never shadow the stable identity with that
	// UUID, including after the stable identity has been deleted.
	out, err = runCLI(t, "tag", "create", "identity target", "--json")
	require.NoError(t, err, out)
	var identityTarget api.Tag
	require.NoError(t, json.Unmarshal([]byte(out), &identityTarget))
	out, err = runCLI(t, "tag", "create", identityTarget.ID, "--json")
	require.NoError(t, err, out)
	var nameShadow api.Tag
	require.NoError(t, json.Unmarshal([]byte(out), &nameShadow))
	assert.NotEqual(t, identityTarget.ID, nameShadow.ID)

	out, err = runCLI(t, "tag", "delete", identityTarget.ID, "--json")
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(out), &deleted))
	assert.Equal(t, identityTarget.ID, deleted.Tag.ID)
	_, err = runCLI(t, "tag", "show", identityTarget.ID, "--json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	out, err = runCLI(t, "tag", "show", nameShadow.ID, "--json")
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(out), &shown))
	assert.Equal(t, nameShadow.ID, shown.ID)
	assert.Equal(t, identityTarget.ID, shown.Name)
}

func TestRefsFindsCurrentHistoricalAndTrashedContent(t *testing.T) {
	_ = setupVaultHome(t)
	initialBytes := []byte("stable lookup content")
	initial := writeSourceFile(t, "lookup.txt", string(initialBytes))
	_, err := runCLI(t, "add", initial, "--dest", "/inbox")
	require.NoError(t, err)
	sum := sha256.Sum256(initialBytes)
	hash := hex.EncodeToString(sum[:])

	out, err := runCLI(t, "refs", hash)
	require.NoError(t, err, out)
	assert.Contains(t, out, "CURRENT")
	assert.Contains(t, out, "yes")
	assert.Contains(t, out, "live")
	assert.Contains(t, out, "/inbox/lookup.txt")

	replacement := writeSourceFile(t, "replacement.txt", "different bytes")
	_, err = runCLI(t, "put", replacement, "/inbox/lookup.txt", "--progress", "plain")
	require.NoError(t, err)
	out, err = runCLI(t, "refs", hash)
	require.NoError(t, err, out)
	assert.Contains(t, out, "no")
	assert.Contains(t, out, "/inbox/lookup.txt")

	_, err = runCLI(t, "rm", "/inbox/lookup.txt")
	require.NoError(t, err)
	out, err = runCLI(t, "refs", hash, "--json")
	require.NoError(t, err, out)
	var page api.ContentReferencePage
	require.NoError(t, json.Unmarshal([]byte(out), &page))
	assert.Equal(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	assert.False(t, page.Items[0].IsCurrent)
	assert.NotEmpty(t, page.Items[0].Node.TrashedAt)
	assert.Empty(t, page.Items[0].Path)

	out, err = runCLI(t, "refs", strings.Repeat("f", 64))
	require.NoError(t, err, out)
	assert.Equal(t, "no authoritative references\n", out)
	_, err = runCLI(t, "refs", "ABC")
	require.ErrorContains(t, err, "canonical lowercase SHA-256")
	_, err = runCLI(t, "refs", hash, "--limit", "0")
	require.ErrorContains(t, err, "--limit must be between 1 and 1000")
}

func TestJobsShowsDaemonStatus(t *testing.T) {
	_ = setupVaultHome(t)
	out, err := runCLI(t, "jobs")
	require.NoError(t, err)
	assert.Contains(t, out, "extract:plain-text")
	assert.Contains(t, out, "running")

	out, err = runCLI(t, "jobs", "--json")
	require.NoError(t, err)
	var got api.JobList
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Items, 1)
	assert.Equal(t, "extract:plain-text", got.Items[0].Name)
	assert.Equal(t, "running", got.Items[0].Status)
}

func TestConfiguredWatchIngestsStableFilesAndRemainsObservable(t *testing.T) {
	home := t.TempDir()
	source := t.TempDir()
	sourcePath := filepath.Join(source, "session.jsonl")
	record := "{\"kind\":\"assistant-session\",\"topic\":\"quasararchive\"}\n"
	require.NoError(t, os.WriteFile(sourcePath, []byte(record), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[server]\nidle_timeout = \"30ms\"\n"+
			"[[watch]]\nname = \"sessions\"\nsource = \""+filepath.ToSlash(source)+"\"\n"+
			"destination = \"/agents\"\nsettle_time = \"50ms\"\nscan_interval = \"10ms\"\n",
	), 0o600))
	t.Setenv("DOCBANK_HOME", home)
	t.Setenv(client.EnvBackgroundDaemon, "1")
	startTestDaemon(t, home)

	require.Eventually(t, func() bool {
		out, err := runCLI(t, "cat", "/agents/session.jsonl")
		return err == nil && out == record
	}, 5*time.Second, 25*time.Millisecond)
	require.Eventually(t, func() bool {
		out, err := runCLI(t, "search", "quasararchive", "--json")
		if err != nil {
			return false
		}
		var report api.SearchReport
		return json.Unmarshal([]byte(out), &report) == nil && len(report.Hits) == 1 &&
			report.Hits[0].Path == "/agents/session.jsonl" && report.Hits[0].Match == "content"
	}, 5*time.Second, 25*time.Millisecond)

	out, err := runCLI(t, "jobs", "--json")
	require.NoError(t, err)
	var got api.JobList
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Items, 2)
	assert.Equal(t, "extract:plain-text", got.Items[0].Name)
	assert.Equal(t, "running", got.Items[0].Status)
	assert.Equal(t, "watch:sessions", got.Items[1].Name)
	assert.Equal(t, "running", got.Items[1].Status)

	// A configured watcher keeps a background daemon alive even when the
	// ordinary request-idle timeout is deliberately tiny.
	time.Sleep(100 * time.Millisecond)
	_, _, found, err := client.Find(t.Context(), home)
	require.NoError(t, err)
	assert.True(t, found)

	sourceBytes, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	assert.Equal(t, record, string(sourceBytes))
}

func TestAddRerunReportsSkips(t *testing.T) {
	_ = setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")

	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)
	out, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)
	assert.Contains(t, out, "scan:")
	assert.Contains(t, out, "ingest:")
	assert.Contains(t, out, "skipped: 1")
}

func TestAddJSONSuppressesProgress(t *testing.T) {
	_ = setupVaultHome(t)
	src := writeSourceFile(t, "json.txt", "quiet automation")

	out, err := runCLI(t, "add", src, "--dest", "/inbox", "--json")
	require.NoError(t, err)
	var report api.IngestReport
	require.NoError(t, json.Unmarshal([]byte(out), &report), out)
	assert.Equal(t, 1, report.Added)
	assert.NotContains(t, out, "scan:")
	assert.NotContains(t, out, "ingest:")
}

func TestAddPreflightReportsWithoutImporting(t *testing.T) {
	_ = setupVaultHome(t)
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "session.jsonl"), []byte("{\"ok\":true}\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(src, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, ".git", "config"), []byte("ignored"), 0o600))

	out, err := runCLI(t, "add", src, "--preflight", "--exclude", ".git")
	require.NoError(t, err)
	assert.Contains(t, out, "files: 1")
	assert.Contains(t, out, "excluded: 1")
	assert.Contains(t, out, ".jsonl")

	_, err = runCLI(t, "ls", "/inbox")
	require.Error(t, err, "preflight must not create the destination")

	out, err = runCLI(t, "add", src, "--preflight", "--exclude", ".git", "--json")
	require.NoError(t, err)
	var report api.IngestPreflightReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, int64(1), report.Files)
	assert.Equal(t, int64(1), report.Excluded)

	out, err = runCLI(t, "add", src, "--exclude", ".git")
	require.NoError(t, err)
	assert.Contains(t, out, "added: 1")
	assert.Contains(t, out, "excluded: 1")

	out, err = runCLI(t, "tree", "/inbox")
	require.NoError(t, err)
	assert.Contains(t, out, "session.jsonl")
	assert.NotContains(t, out, ".git")
}

func TestAddExcludeCommaIsLiteral(t *testing.T) {
	_ = setupVaultHome(t)
	src := t.TempDir()
	for _, rel := range []string{"cache,tmp/ignored.txt", "cache/kept.txt", "tmp/kept.txt"} {
		path := filepath.Join(src, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(rel), 0o600))
	}

	out, err := runCLI(t, "add", src, "--preflight", "--exclude", "cache,tmp", "--json")
	require.NoError(t, err)
	var report api.IngestPreflightReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, int64(2), report.Files,
		"comma-containing entry must be one literal rule; cache and tmp remain selected")
	assert.Equal(t, int64(1), report.Excluded)

	// The literal array flag appends repeated occurrences in production, but
	// in-process CLI executions must still reset it between invocations.
	out, err = runCLI(t, "add", src, "--preflight", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, int64(3), report.Files)
	assert.Zero(t, report.Excluded)
}

func TestAddMissingSourceFails(t *testing.T) {
	_ = setupVaultHome(t)

	_, err := runCLI(t, "add", "/no/such/file", "--dest", "/x")
	require.Error(t, err)
}

func TestAddRejectsInvalidUTF8SourceBeforeEncoding(t *testing.T) {
	_ = setupVaultHome(t)
	invalidPath := filepath.Join(t.TempDir(), string([]byte{'b', 'a', 'd', 0xff}))

	_, err := runCLI(t, "add", invalidPath)
	require.ErrorContains(t, err, "is not valid UTF-8")
}

func TestCatRejectsDirectory(t *testing.T) {
	_ = setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	_, err = runCLI(t, "cat", "/inbox")
	require.Error(t, err)
}

func TestMvIntoDirAndRename(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	// Rename in place (dest is a non-existent name in an existing dir).
	_, err = runCLI(t, "mv", "/inbox/a.txt", "/inbox/b.txt")
	require.NoError(t, err)
	c, err := client.Ensure(context.Background())
	require.NoError(t, err)
	movedByID, err := c.Stat(context.Background(), "/inbox/b.txt")
	require.NoError(t, err)

	out, err := runCLI(t, "ls", "/inbox")
	require.NoError(t, err)
	assert.NotContains(t, out, "a.txt", "renamed source must be vacated")

	// Move into an existing directory, keeping the name.
	seed := writeSourceFile(t, "seed.txt", "s")
	_, err = runCLI(t, "add", seed, "--dest", "/filed")
	require.NoError(t, err)
	_, err = runCLI(t, "mv", formatNodeSelector(movedByID.ID), "/filed")
	require.NoError(t, err)

	out, err = runCLI(t, "ls", "/filed")
	require.NoError(t, err)
	assert.Contains(t, out, "b.txt")

	out, err = runCLI(t, "ls", "/inbox")
	require.NoError(t, err)
	assert.NotContains(t, out, "b.txt", "moved source must be vacated")
}

func TestMvOntoSelfFails(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	_, err = runCLI(t, "mv", "/inbox/a.txt", "/inbox/a.txt")
	require.ErrorIs(t, err, store.ErrExists)
}

func TestRmRestoreRoundTrip(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	c, err := client.Ensure(context.Background())
	require.NoError(t, err)
	node, err := c.Stat(context.Background(), "/inbox/a.txt")
	require.NoError(t, err)

	// rm prints the same copyable selector accepted by restore.
	out, err := runCLI(t, "rm", formatNodeSelector(node.ID))
	require.NoError(t, err)
	assert.Contains(t, out, "trashed")
	m := regexp.MustCompile(`\[(id:\d+)\]`).FindStringSubmatch(out)
	require.Len(t, m, 2)

	out, err = runCLI(t, "trash", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "a.txt")

	_, err = runCLI(t, "restore", m[1])
	require.NoError(t, err)
	out, err = runCLI(t, "ls", "/inbox")
	require.NoError(t, err)
	assert.Contains(t, out, "a.txt")
}

func TestTrashedIDSelectorsRespectLiveCommandBoundary(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "record.txt", "retained while trashed")
	_, err := runCLI(t, "add", src, "--dest", "/archive")
	require.NoError(t, err)
	c, err := client.Ensure(t.Context())
	require.NoError(t, err)
	dir, err := c.Stat(t.Context(), "/archive")
	require.NoError(t, err)
	file, err := c.Stat(t.Context(), "/archive/record.txt")
	require.NoError(t, err)
	dirSelector := formatNodeSelector(dir.ID)
	fileSelector := formatNodeSelector(file.ID)

	_, err = runCLI(t, "rm", dirSelector)
	require.NoError(t, err)

	// Stable identity remains useful for read-only inspection in trash.
	out, err := runCLI(t, "cat", fileSelector)
	require.NoError(t, err, out)
	assert.Equal(t, "retained while trashed", out)
	out, err = runCLI(t, "versions", "list", fileSelector, "--json")
	require.NoError(t, err, out)
	var versions api.ContentVersionPage
	require.NoError(t, json.Unmarshal([]byte(out), &versions))
	assert.Equal(t, 1, versions.Total)

	_, err = runCLI(t, "tag", "create", "review")
	require.NoError(t, err)
	for _, args := range [][]string{
		{"ls", dirSelector},
		{"tree", dirSelector},
		{"mv", fileSelector, "/record.txt"},
		{"rm", fileSelector},
		{"edit", fileSelector, "--editor", "true"},
		{"tag", "assign", "review", fileSelector},
		{"versions", "prune", fileSelector, "--all-prior"},
	} {
		_, runErr := runCLI(t, args...)
		require.ErrorContains(t, runErr, "node is trashed", args)
		assert.Equal(t, exitNotFound, commandExitCode(runErr, true), args)
	}
}

func TestTreeMutationJSONReceipts(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	out, err := runCLI(t, "mv", "/inbox/a.txt", "/inbox/b.txt", "--json")
	require.NoError(t, err, out)
	var moved api.Node
	require.NoError(t, json.Unmarshal([]byte(out), &moved))
	assert.Equal(t, "/inbox/b.txt", moved.Path)
	assert.Equal(t, "b.txt", moved.Name)
	assert.Empty(t, moved.TrashedAt)

	out, err = runCLI(t, "rm", "/inbox/b.txt", "--json")
	require.NoError(t, err, out)
	var trashed api.Node
	require.NoError(t, json.Unmarshal([]byte(out), &trashed))
	assert.Equal(t, moved.ID, trashed.ID)
	assert.Equal(t, "/inbox/b.txt", trashed.Path, "trash receipt retains the pre-trash path")
	assert.NotEmpty(t, trashed.TrashedAt)
	assert.Greater(t, trashed.Revision, moved.Revision)

	out, err = runCLI(t, "restore", strconv.FormatInt(trashed.ID, 10), "--json")
	require.NoError(t, err, out)
	var restored api.Node
	require.NoError(t, json.Unmarshal([]byte(out), &restored))
	assert.Equal(t, moved.ID, restored.ID)
	assert.Equal(t, "/inbox/b.txt", restored.Path)
	assert.Empty(t, restored.TrashedAt)
	assert.Greater(t, restored.Revision, trashed.Revision)
}

func TestSearchCLI(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "insurance-2026.txt", "policy")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	out, err := runCLI(t, "search", "insurance")
	require.NoError(t, err)
	assert.Contains(t, out, "/inbox/insurance-2026.txt")
}

func TestSearchCLIReportsTruncation(t *testing.T) {
	setupVaultHome(t)
	srcA := writeSourceFile(t, "report-a.txt", "alpha")
	srcB := writeSourceFile(t, "report-b.txt", "bravo")
	_, err := runCLI(t, "add", srcA, srcB, "--dest", "/inbox")
	require.NoError(t, err)

	out, err := runCLI(t, "search", "report", "--limit", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "more than 1 result")
	assert.Contains(t, out, "increase --limit")

	_, err = runCLI(t, "search", "report", "--limit", "0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "between 1 and 1000")
}

func TestTrashEmpty(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)
	_, err = runCLI(t, "rm", "/inbox/a.txt")
	require.NoError(t, err)

	out, err := runCLI(t, "trash", "empty")
	require.NoError(t, err)
	assert.Contains(t, out, "1 trashed root")
	assert.Contains(t, out, "dry run")

	// Dry run leaves the node in trash.
	out, err = runCLI(t, "trash", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "a.txt")

	out, err = runCLI(t, "trash", "empty", "--run")
	require.NoError(t, err)
	assert.Contains(t, out, "deleted 1")

	out, err = runCLI(t, "trash", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "trash is empty")
}

func TestTreeRejectsFileWithoutOutput(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	out, err := runCLI(t, "tree", "/inbox/a.txt")
	require.ErrorIs(t, err, store.ErrNotDir)
	assert.Empty(t, out, "tree must not emit partial output before failing")
}

func TestTrashEmptyRejectsNegativeAge(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)
	_, err = runCLI(t, "rm", "/inbox/a.txt")
	require.NoError(t, err)

	_, err = runCLI(t, "trash", "empty", "--older-than=-1h")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "negative")

	// Nothing was deleted.
	out, err := runCLI(t, "trash", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "a.txt")
}

func TestGcReclaimsUnreachableBlobs(t *testing.T) {
	home := setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	// Nothing to collect while the node is live.
	out, err := runCLI(t, "gc")
	require.NoError(t, err)
	assert.Contains(t, out, "0 candidate")

	_, err = runCLI(t, "rm", "/inbox/a.txt")
	require.NoError(t, err)
	_, err = runCLI(t, "trash", "empty", "--run")
	require.NoError(t, err)

	// Dry-run reports but does not delete.
	out, err = runCLI(t, "gc")
	require.NoError(t, err)
	assert.Contains(t, out, "1 candidate")
	blobFiles, err := filepath.Glob(filepath.Join(home, "blobs", "??", "*"))
	require.NoError(t, err)
	assert.Len(t, blobFiles, 1)

	// --run deletes file and rows.
	out, err = runCLI(t, "gc", "--run")
	require.NoError(t, err)
	assert.Contains(t, out, "reclaimed")
	blobFiles, err = filepath.Glob(filepath.Join(home, "blobs", "??", "*"))
	require.NoError(t, err)
	assert.Empty(t, blobFiles)

	// Re-run converges.
	out, err = runCLI(t, "gc")
	require.NoError(t, err)
	assert.Contains(t, out, "0 candidate")
}

func TestStorageStatusHumanAndJSON(t *testing.T) {
	_ = setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	out, err := runCLI(t, "storage", "status")
	require.NoError(t, err)
	assert.Contains(t, out, "loose: 1 blob(s), 5 byte(s)")
	assert.Contains(t, out, "packed: 0 live blob(s) in 0 pack(s)")

	out, err = runCLI(t, "storage", "status", "--json")
	require.NoError(t, err)
	var status api.StorageStatus
	require.NoError(t, json.Unmarshal([]byte(out), &status))
	assert.Equal(t, 1, status.LooseBlobs)
	assert.Equal(t, int64(5), status.LooseBytes)
}

func TestBackupInitCreateListVerifyCLI(t *testing.T) {
	home := t.TempDir()
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[backup]\nrepo = \""+filepath.ToSlash(repoPath)+"\"\n"), 0o600))
	t.Setenv("DOCBANK_HOME", home)
	startTestDaemon(t, home)

	out, err := runCLI(t, "backup", "init")
	require.NoError(t, err)
	assert.Contains(t, out, "initialized backup repository")

	src := writeSourceFile(t, "archive.txt", "durable backup")
	_, err = runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)
	out, err = runCLI(t, "backup", "create", "--tag", "first", "--jobs", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "created snapshot")
	assert.Contains(t, out, "1 file(s), 1 blob(s)")
	for _, stage := range []string{"freeze:", "metadata:", "attachments:", "seal:"} {
		assert.Contains(t, out, stage)
	}

	out, err = runCLI(t, "backup", "create", "--tag", "json", "--jobs", "1", "--json")
	require.NoError(t, err)
	var created api.BackupSnapshot
	require.NoError(t, json.Unmarshal([]byte(out), &created), "--json must contain no progress lines")
	assert.Equal(t, "json", created.Tag)

	out, err = runCLI(t, "backup", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "SNAPSHOT")
	assert.Contains(t, out, "first")
	out, err = runCLI(t, "backup", "list", "--json")
	require.NoError(t, err)
	var listed api.BackupSnapshotList
	require.NoError(t, json.Unmarshal([]byte(out), &listed))
	require.Len(t, listed.Items, 2)
	assert.Equal(t, backupapp.MetadataFormat, listed.Items[0].MetadataFormat)

	out, err = runCLI(t, "backup", "verify", "--all", "--jobs", "1", "--progress", "plain")
	require.NoError(t, err)
	assert.Contains(t, out, "verify:")
	assert.Contains(t, out, "verified 2 snapshot(s)")
	assert.Contains(t, out, "0 problem(s)")

	out, err = runCLI(t, "backup", "verify", created.ID, "--quick", "--json")
	require.NoError(t, err)
	var verification api.BackupVerifyReport
	require.NoError(t, json.Unmarshal([]byte(out), &verification),
		"--json must contain no progress lines")
	assert.Equal(t, []string{created.ID}, verification.Snapshots)
	assert.Positive(t, verification.BytesRead, "quick verification still reads metadata")

	_, err = runCLI(t, "backup", "verify", created.ID, "--all")
	require.ErrorContains(t, err, "mutually exclusive")

	restoreTarget := filepath.Join(t.TempDir(), "human-restore")
	out, err = runCLI(t, "backup", "restore", created.ID, "--target", restoreTarget,
		"--jobs", "1", "--progress", "plain")
	require.NoError(t, err)
	for _, stage := range []string{
		"metadata:", "attachments:", "integrity:", "statistics:",
	} {
		assert.Contains(t, out, stage)
	}
	assert.Contains(t, out, "restored snapshot "+created.ID)
	assert.Contains(t, out, "1 packed in 1 pack(s), 0 loose")
	assert.Contains(t, out, "proof: content verified, SQLite integrity ok, manifest stats match")

	jsonRestoreTarget := filepath.Join(t.TempDir(), "json-restore")
	out, err = runCLI(t, "backup", "restore", "--target", jsonRestoreTarget,
		"--jobs", "1", "--json")
	require.NoError(t, err)
	var restoreReport api.BackupRestoreReport
	require.NoError(t, json.Unmarshal([]byte(out), &restoreReport),
		"--json must contain no progress lines")
	assert.Equal(t, created.ID, restoreReport.SnapshotID)
	targetInfo, err := os.Stat(jsonRestoreTarget)
	require.NoError(t, err)
	reportedTargetInfo, err := os.Stat(restoreReport.Target)
	require.NoError(t, err)
	assert.True(t, os.SameFile(targetInfo, reportedTargetInfo),
		"report target must identify the requested directory")
	assert.True(t, restoreReport.Proof.ContentVerified)
	assert.True(t, restoreReport.Proof.SQLiteIntegrity)
	assert.True(t, restoreReport.Proof.ManifestStats)

	overwriteTarget := filepath.Join(t.TempDir(), "overwrite-restore")
	require.NoError(t, os.MkdirAll(overwriteTarget, 0o700))
	marker := filepath.Join(overwriteTarget, "keep.txt")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0o600))
	_, err = runCLI(t, "backup", "restore", "--target", overwriteTarget, "--jobs", "1", "--json")
	require.ErrorContains(t, err, "not empty")
	markerBytes, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(markerBytes))
	out, err = runCLI(t, "backup", "restore", "--target", overwriteTarget,
		"--overwrite", "--jobs", "1", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &restoreReport))
	markerBytes, err = os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(markerBytes), "overwrite merges unrelated files")

	repo, err := backup.Open(repoPath)
	require.NoError(t, err)
	verified, err := backup.Verify(t.Context(), repo, backupapp.New("test"), backup.VerifyOptions{})
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)

	manifests, err := repo.ListSnapshots()
	require.NoError(t, err)
	require.Len(t, manifests, 2)
	manifest := manifests[0]
	require.NotEmpty(t, manifest.NewPacks)
	packID := manifest.NewPacks[0]
	packPath := repo.Path("packs", packID[:2], packID+packstore.PackExt)
	packBytes, err := os.ReadFile(packPath)
	require.NoError(t, err)
	packBytes[len(packBytes)/3] ^= 1
	require.NoError(t, os.WriteFile(packPath, packBytes, 0o600))

	out, err = runCLI(t, "backup", "verify", manifest.SnapshotID, "--jobs", "1", "--json")
	require.ErrorContains(t, err, "found")
	require.NoError(t, json.Unmarshal([]byte(out), &verification),
		"failed JSON proof must remain one machine-readable report")
	assert.NotEmpty(t, verification.Problems)
	assert.Equal(t, manifest.SnapshotID, verification.Problems[0].SnapshotID)
}

func TestStoragePackBudgetAndJSON(t *testing.T) {
	_ = setupVaultHome(t)
	for name, content := range map[string]string{"a.txt": "alpha", "b.txt": "bravo"} {
		src := writeSourceFile(t, name, content)
		_, err := runCLI(t, "add", src, "--dest", "/inbox")
		require.NoError(t, err)
	}

	out, err := runCLI(t, "storage", "pack", "--max-bytes", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "packed 1 blob(s)")
	assert.Contains(t, out, "byte budget exhausted")

	out, err = runCLI(t, "storage", "pack", "--json")
	require.NoError(t, err)
	var report api.StoragePackReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 1, report.BlobsPacked)
	assert.False(t, report.BudgetExhausted)

	out, err = runCLI(t, "storage", "status", "--json")
	require.NoError(t, err)
	var status api.StorageStatus
	require.NoError(t, json.Unmarshal([]byte(out), &status))
	assert.Zero(t, status.LooseBlobs)
	assert.Equal(t, int64(2), status.PackedBlobs)
}

func TestStorageRepackJSON(t *testing.T) {
	_ = setupVaultHome(t)
	for name, content := range map[string]string{
		"keep.txt": "keep", "drop-a.txt": "drop a", "drop-b.txt": "drop b",
	} {
		src := writeSourceFile(t, name, content)
		_, err := runCLI(t, "add", src, "--dest", "/inbox")
		require.NoError(t, err)
	}
	_, err := runCLI(t, "storage", "pack")
	require.NoError(t, err)
	for _, path := range []string{"/inbox/drop-a.txt", "/inbox/drop-b.txt"} {
		_, err = runCLI(t, "rm", path)
		require.NoError(t, err)
	}
	_, err = runCLI(t, "trash", "empty", "--run")
	require.NoError(t, err)
	_, err = runCLI(t, "gc", "--run")
	require.NoError(t, err)

	out, err := runCLI(t, "storage", "repack", "--min-age", "1ns",
		"--min-dead-bytes", "1", "--json")
	require.NoError(t, err)
	var report api.StorageRepackReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 1, report.PacksRewritten)
	assert.Equal(t, 1, report.PacksRemoved)
	assert.Equal(t, 1, report.BlobsRepacked)

	out, err = runCLI(t, "storage", "status", "--json")
	require.NoError(t, err)
	var status api.StorageStatus
	require.NoError(t, json.Unmarshal([]byte(out), &status))
	assert.Zero(t, status.DeadPackedBytes)
}

func TestDeletionHelpSeparatesTrashGCAndRepack(t *testing.T) {
	out, err := runCLI(t, "rm", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "rm never permanently deletes metadata or reclaims content")

	out, err = runCLI(t, "trash", "empty", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "Content bytes remain")
	assert.Contains(t, out, "packed space then requires repack")

	out, err = runCLI(t, "gc", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "loose files are reclaimed immediately")
	assert.Contains(t, out, "requires a separate storage repack")
}

func TestVerifyDetectsMissingAndCorrupt(t *testing.T) {
	home := setupVaultHome(t)
	srcA := writeSourceFile(t, "a.txt", "alpha")
	srcB := writeSourceFile(t, "b.txt", "beta")
	_, err := runCLI(t, "add", srcA, srcB, "--dest", "/inbox")
	require.NoError(t, err)

	out, err := runCLI(t, "verify")
	require.NoError(t, err)
	assert.Contains(t, out, "2 blob(s) ok")

	alphaSum := sha256.Sum256([]byte("alpha"))
	alphaHash := hex.EncodeToString(alphaSum[:])
	betaSum := sha256.Sum256([]byte("beta"))
	betaHash := hex.EncodeToString(betaSum[:])

	// Blobs are hash-named; find each by its expected hash rather than
	// assuming Glob's return order matches insertion order.
	blobFiles, err := filepath.Glob(filepath.Join(home, "blobs", "??", "*"))
	require.NoError(t, err)
	require.Len(t, blobFiles, 2)
	var alphaPath, betaPath string
	for _, p := range blobFiles {
		switch filepath.Base(p) {
		case alphaHash:
			alphaPath = p
		case betaHash:
			betaPath = p
		}
	}
	require.NotEmpty(t, alphaPath, "alpha blob not found among %v", blobFiles)
	require.NotEmpty(t, betaPath, "beta blob not found among %v", blobFiles)

	// Tamper alpha's blob, delete beta's.
	require.NoError(t, os.WriteFile(alphaPath, []byte("tampered"), 0o600))
	require.NoError(t, os.Remove(betaPath))

	out, err = runCLI(t, "verify")
	require.Error(t, err)
	assert.Contains(t, out, "corrupt: "+alphaHash)
	assert.Contains(t, out, "missing: "+betaHash)

	var stdout, stderr bytes.Buffer
	code := runProcess([]string{"verify"}, &stdout, &stderr)
	assert.Equal(t, exitIntegrity, code)
	assert.Contains(t, stdout.String(), "2 problem(s)")
	assert.Contains(t, stderr.String(), "verify found 2 problem(s)")
}

// A validation failure exits before RunE, so nothing command-side can reset
// the parsed flag; the runCLI wrapper must prevent the stale --dest from
// leaking into the next invocation.
func TestAddDestDoesNotLeakAcrossValidationFailure(t *testing.T) {
	setupVaultHome(t)

	_, err := runCLI(t, "add", "--dest", "/leaked") // no sources: MinimumNArgs fails
	require.Error(t, err)

	src := writeSourceFile(t, "a.txt", "alpha")
	_, err = runCLI(t, "add", src) // no --dest: must use the default /inbox
	require.NoError(t, err)

	out, err := runCLI(t, "ls", "/inbox")
	require.NoError(t, err)
	assert.Contains(t, out, "a.txt")
	_, err = runCLI(t, "ls", "/leaked")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestGcReclaimsUntrackedBlobFiles(t *testing.T) {
	home := setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	// Manufacture the failed-transaction / crash shape: a durable blob file
	// with no blobs row. Row-based queries can never see it.
	content := []byte("orphaned bytes")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	shard := filepath.Join(home, "blobs", hash[:2])
	require.NoError(t, os.MkdirAll(shard, 0o700))
	orphan := filepath.Join(shard, hash)
	require.NoError(t, os.WriteFile(orphan, content, 0o600))

	// Dry run reports it without deleting.
	out, err := runCLI(t, "gc")
	require.NoError(t, err)
	assert.Contains(t, out, "1 untracked")
	assert.FileExists(t, orphan)

	out, err = runCLI(t, "gc", "--run")
	require.NoError(t, err)
	assert.Contains(t, out, "reclaimed")
	assert.NoFileExists(t, orphan)

	// The tracked, live blob is untouched.
	catOut, err := runCLI(t, "cat", "/inbox/a.txt")
	require.NoError(t, err)
	assert.Equal(t, "alpha", catOut)
}
