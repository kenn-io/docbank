package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
)

func TestNativeReadCLIChild(t *testing.T) {
	t.Helper()
	if os.Getenv("DOCBANK_NATIVE_READ_CHILD") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Exit(runProcess(os.Args[i+1:], os.Stdout, os.Stderr))
		}
	}
	os.Exit(99)
}

func nativeReadSubprocess(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	return nativeReadSubprocessWithSession(t, "", args...)
}

func nativeReadSubprocessWithSession(t *testing.T, sessionFile string, args ...string) (string, string, error) {
	t.Helper()
	bin, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(bin, append([]string{"-test.run=^TestNativeReadCLIChild$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "DOCBANK_NATIVE_READ_CHILD=1")
	if sessionFile != "" {
		cmd.Env = append(cmd.Env, "DOCBANK_AGENT_SESSION_FILE="+sessionFile)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

func TestNativeReadCLISubprocessCatalogCoverageAndBounds(t *testing.T) {
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: plaintext.MaxDocumentBytes})
	require.NoError(t, err)
	cfg := plaintextProcessingConfig(provider.Descriptor().Fingerprint)
	cfg.Server.APIKey = "synthetic-cli-read-key"
	root := t.TempDir()
	var encoded bytes.Buffer
	require.NoError(t, toml.NewEncoder(&encoded).Encode(map[string]any{
		"server":             map[string]string{"api_key": cfg.Server.APIKey},
		"rendition_profiles": cfg.RenditionProfiles, "processing_profiles": cfg.ProcessingProfiles,
		"retrieval_profiles": cfg.RetrievalProfiles,
	}))
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), encoded.Bytes(), 0o600))
	t.Setenv("DOCBANK_HOME", root)
	startTestDaemon(t, root)
	for _, name := range []string{"alpha.txt", "beta.txt"} {
		file := writeSourceFile(t, name, "synthetic "+name)
		_, stderr, err := nativeReadSubprocess(t, "add", file, "--dest", "/docs")
		require.NoError(t, err, stderr)
	}

	first, stderr, err := nativeReadSubprocess(t, "documents", "list", "--path-prefix", "/docs", "--page-size", "1", "--json")
	require.NoError(t, err, stderr)
	assert.Empty(t, stderr)
	var page api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(first), &page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, "/docs/alpha.txt", page.Items[0].Path)
	assert.NotEmpty(t, page.NextCursor)
	otherVersionID := page.Items[0].ContentVersionID

	second, stderr, err := nativeReadSubprocess(t, "documents", "list", "--path-prefix", "/docs", "--page-size", "1", "--cursor", page.NextCursor, "--json")
	require.NoError(t, err, stderr)
	assert.Empty(t, stderr)
	require.NoError(t, json.Unmarshal([]byte(second), &page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, "/docs/beta.txt", page.Items[0].Path)
	versionID := page.Items[0].ContentVersionID
	sessionFile := filepath.Join(t.TempDir(), "agent-session.json")
	issued, stderr, err := nativeReadSubprocess(t, "agent-session", "issue", "--source-id", versionID, "--output", sessionFile)
	require.NoError(t, err, stderr)
	fields := strings.Fields(issued)
	require.GreaterOrEqual(t, len(fields), 2)
	sessionID := fields[1]
	scopedPageJSON, stderr, err := nativeReadSubprocessWithSession(t, sessionFile,
		"documents", "list", "--path-prefix", "/docs", "--source-version", versionID, "--json")
	require.NoError(t, err, stderr)
	var scopedPage api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(scopedPageJSON), &scopedPage))
	require.Len(t, scopedPage.Items, 1)
	require.Equal(t, versionID, scopedPage.Items[0].ContentVersionID)
	for _, args := range [][]string{
		{"documents", "list", "--path-prefix", "/docs", "--json"},
		{"documents", "list", "--path-prefix", "/docs", "--source-version", otherVersionID, "--json"},
	} {
		stdout, _, readErr := nativeReadSubprocessWithSession(t, sessionFile, args...)
		require.Error(t, readErr)
		require.Empty(t, stdout)
	}
	_, stderr, err = nativeReadSubprocess(t, "agent-session", "revoke", sessionID)
	require.NoError(t, err, stderr)
	stdout, _, err := nativeReadSubprocessWithSession(t, sessionFile,
		"documents", "list", "--path-prefix", "/docs", "--source-version", versionID, "--json")
	require.Error(t, err)
	require.Empty(t, stdout)

	infoJSON, stderr, err := nativeReadSubprocess(t, "info", "--json")
	require.NoError(t, err, stderr)
	var info api.VaultInfo
	require.NoError(t, json.Unmarshal([]byte(infoJSON), &info))
	coverageJSON, stderr, err := nativeReadSubprocess(t, "processing", "coverage", "--profile", "private-text", "--vault-id", info.VaultID, "--source-version", versionID, "--json")
	require.NoError(t, err, stderr)
	assert.Empty(t, stderr)
	var coverage api.CoverageReport
	require.NoError(t, json.Unmarshal([]byte(coverageJSON), &coverage))
	assert.Equal(t, info.VaultID, coverage.VaultUID)
	assert.Equal(t, 1, coverage.Renditions.Total)

	for _, args := range [][]string{
		{"documents", "list", "--page-size", "0", "--json"},
		{"documents", "list", "--page-size", "251", "--json"},
		{"documents", "list", "--path-prefix", strings.Repeat("é", 9000), "--json"},
		{"processing", "coverage", "--profile", "private-text", "--vault-id", info.VaultID, "--source-version", versionID, "--source-version", versionID, "--json"},
	} {
		stdout, stderr, err := nativeReadSubprocess(t, args...)
		require.Error(t, err, args)
		assert.Empty(t, stdout, args)
		assert.NotEmpty(t, stderr, args)
	}

	stdout, stderr, err = nativeReadSubprocess(t, "rendition", "text", "--vault-id", info.VaultID, "--node-id", strconv.FormatInt(page.Items[0].NodeID, 10),
		"--source-version", versionID, "--attachment-id", strings.Repeat("a", 64), "--max-chars", "0", "--json")
	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "max-chars")

	nodeID := strconv.FormatInt(page.Items[0].NodeID, 10)
	planJSON, stderr, err := nativeReadSubprocess(t, "processing", "plan", "id:"+nodeID, "--profile", "private-text", "--json")
	require.NoError(t, err, stderr)
	var plan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(planJSON), &plan))
	_, stderr, err = nativeReadSubprocess(t, "processing", "build", "id:"+nodeID, "--profile", "private-text",
		"--plan-fingerprint", plan.Fingerprint, "--consent", "--json")
	require.NoError(t, err, stderr)
	pageJSON, stderr, err := nativeReadSubprocess(t, "documents", "list", "--path-prefix", "/docs", "--page-size", "2", "--json")
	require.NoError(t, err, stderr)
	require.NoError(t, json.Unmarshal([]byte(pageJSON), &page))
	require.Len(t, page.Items, 2)
	require.Len(t, page.Items[1].ActiveRenditions, 1)
	attachmentID := page.Items[1].ActiveRenditions[0].AttachmentID

	text, stderr, err := nativeReadSubprocess(t, "rendition", "text", "--vault-id", info.VaultID, "--node-id", nodeID,
		"--source-version", versionID, "--attachment-id", attachmentID, "--max-chars", "9")
	require.NoError(t, err, stderr)
	assert.Empty(t, stderr)
	assert.Equal(t, "---\ndocba", text)

	windowJSON, stderr, err := nativeReadSubprocess(t, "rendition", "text", "--vault-id", info.VaultID, "--node-id", nodeID,
		"--source-version", versionID, "--attachment-id", attachmentID, "--offset", "0", "--max-chars", "9", "--json")
	require.NoError(t, err, stderr)
	assert.Empty(t, stderr)
	var window api.RenditionTextWindow
	require.NoError(t, json.Unmarshal([]byte(windowJSON), &window))
	assert.Equal(t, "---\ndocba", window.Text)
	assert.Equal(t, versionID, window.ContentVersionID)
	assert.Equal(t, 0, window.ActualStart)

	stdout, stderr, err = nativeReadSubprocess(t, "rendition", "text", "--vault-id", info.VaultID, "--node-id", nodeID,
		"--source-version", versionID, "--attachment-id", strings.Repeat("a", 64), "--max-chars", "9")
	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.NotEmpty(t, stderr)
}
