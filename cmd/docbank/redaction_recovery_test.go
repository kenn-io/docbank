package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestNativePDFRedactionRecoveryCLI(t *testing.T) {
	if !pagerender.SupportedPlatform() {
		t.Skip("page runtime requires Linux amd64")
	}
	if os.Getenv("DOCBANK_TEST_PDFTOPPM") == "" {
		if _, err := exec.LookPath("pdftoppm"); err != nil {
			t.Skip("Poppler unavailable")
		}
	}
	const child = "DOCBANK_TEST_NATIVE_REDACTION_RECOVERY_CLI"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		executable, err := os.Executable()
		require.NoError(t, err)
		command := exec.CommandContext(ctx, executable,
			"-test.run=^TestNativePDFRedactionRecoveryCLI$", "-test.v", "-test.timeout=5m")
		command.Env = append(os.Environ(), child+"=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		t.Log(string(output))
		return
	}
	qualifyRedactionRecovery(t)
}

func qualifyRedactionRecovery(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	configuration := map[string]any{
		"page_runtime": redactiontest.PageProfile(t),
		"backup":       map[string]string{"repo": filepath.Join(t.TempDir(), "backup")},
	}
	writeConfig := func() {
		var encoded bytes.Buffer
		require.NoError(t, toml.NewEncoder(&encoded).Encode(configuration))
		require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), encoded.Bytes(), 0600))
	}
	writeConfig()
	stop := startServe(t)
	waitForDaemon(t, root)
	_, err := runCLI(t, "backup", "init")
	require.NoError(t, err)
	f := importNativeRedaction(t,
		redactiontest.PDF(t, []string{"SECRET KEEP SECRET"}, ""), "source.pdf")
	review := f.resolve(t, []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{
		Kind: "text", MapSHA256: f.prep.Map.SHA256, Span: &redaction.Span{End: 6},
	}}})
	archive, rendered := f.render(t, "redacted.zip")
	files, receipt := readNativeRedactionZIP(t, archive)
	require.Equal(t, "[REDACTED] KEEP SECRET\f", string(files["redacted.txt"]))
	require.Equal(t, review.SHA256, receipt.ReviewSHA256)
	require.Equal(t, review.Selection, receipt.Selection)
	require.LessOrEqual(t, rendered.Size, blob.MaxPackedBlobBytes)
	_, err = runCLI(t, "add", archive, "--dest", "/produced")
	require.NoError(t, err)
	identity := redactionArchiveNode(t, "/produced/redacted.zip")
	require.Equal(t, rendered.SHA256, identity.BlobHash)
	require.Equal(t, rendered.Size, identity.Size)
	maintainRedactionRecovery(t, f.prep.Selection.Source.VersionID)
	stop()
	delete(configuration, "page_runtime")
	writeConfig()
	stop = startServe(t)
	waitForDaemon(t, root)
	restarted := retrieveRedactionArchive(t, identity)
	snapshot := backupReportSnapshot(t, "redacted-archive")
	target := restoreReportSnapshot(t, snapshot)
	stop()

	t.Setenv("DOCBANK_HOME", target)
	stop = startServe(t)
	waitForDaemon(t, target)
	restoredIdentity := redactionArchiveNode(t, formatNodeSelector(identity.ID))
	require.Equal(t, identity.ID, restoredIdentity.ID)
	require.Equal(t, identity.CurrentVersionID, restoredIdentity.CurrentVersionID)
	require.Equal(t, identity.BlobHash, restoredIdentity.BlobHash)
	require.Equal(t, identity.Size, restoredIdentity.Size)
	_, err = runCLI(t, "versions", "show", f.prep.Selection.Source.VersionID)
	require.Equal(t, exitNotFound, commandExitCode(err, true))
	restored := retrieveRedactionArchive(t, restoredIdentity)
	stop()

	for _, path := range []string{restarted, restored} {
		output, err := runCLI(t, "redaction", "verify", f.preparation, f.masks, f.review, path, "--json")
		require.NoError(t, err)
		var checked processing.RedactionArchive
		require.NoError(t, json.Unmarshal([]byte(output), &checked))
		require.Equal(t, rendered, checked)
	}
}

func maintainRedactionRecovery(t *testing.T, sourceVersion string) {
	t.Helper()
	for _, args := range [][]string{
		{"rm", "/redaction/source.pdf"}, {"trash", "empty", "--run"}, {"gc", "--run"},
	} {
		_, err := runCLI(t, args...)
		require.NoError(t, err)
	}
	_, err := runCLI(t, "versions", "show", sourceVersion)
	require.Equal(t, exitNotFound, commandExitCode(err, true))
	output, err := runCLI(t, "storage", "pack", "--json")
	require.NoError(t, err)
	var packed api.StoragePackReport
	require.NoError(t, json.Unmarshal([]byte(output), &packed))
	require.Positive(t, packed.PacksSealed)
	require.Positive(t, packed.BlobsPacked)
	require.Positive(t, packed.BytesPacked)
	_, err = runCLI(t, "gc", "--run")
	require.NoError(t, err)
}

func redactionArchiveNode(t *testing.T, selector string) api.Node {
	t.Helper()
	output, err := runCLI(t, "stat", selector, "--json")
	require.NoError(t, err)
	var node api.Node
	require.NoError(t, json.Unmarshal([]byte(output), &node))
	return node
}

func retrieveRedactionArchive(t *testing.T, identity api.Node) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "retrieved.zip")
	output, err := runCLI(t, "get", formatNodeSelector(identity.ID), path, "--json")
	require.NoError(t, err)
	var receipt getReceipt
	require.NoError(t, json.Unmarshal([]byte(output), &receipt))
	require.Equal(t, identity.ID, receipt.NodeID)
	require.Equal(t, identity.CurrentVersionID, receipt.VersionID)
	require.Equal(t, identity.BlobHash, receipt.BlobHash)
	require.Equal(t, identity.Size, receipt.Size)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, identity.Size, int64(len(data)))
	require.Equal(t, identity.BlobHash, redactionPDFHash(data))
	return path
}
