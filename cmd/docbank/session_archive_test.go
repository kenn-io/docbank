package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

func TestAgentSessionArchiveSurvivesPackedBackupRestore(t *testing.T) {
	home := t.TempDir()
	source := t.TempDir()
	repo := filepath.Join(t.TempDir(), "backup")
	relativeSource := filepath.Join("codex", "project-alpha", "2026-07", "session-01.jsonl")
	sourcePath := filepath.Join(source, relativeSource)
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o700))

	first := []byte("{\"role\":\"user\",\"content\":\"synthetic first turn\"}\n")
	final := []byte("{\"role\":\"user\",\"content\":\"synthetic first turn\"}\n" +
		"{\"role\":\"assistant\",\"content\":\"quasar archive complete\"}\n")
	oldMTime := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	writeClosedSession := func(content []byte) {
		require.NoError(t, os.WriteFile(sourcePath, content, 0o600))
		require.NoError(t, os.Chtimes(sourcePath, oldMTime, oldMTime))
	}
	writeClosedSession(first)

	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[server]\nidle_timeout = \"30ms\"\n"+
			"[backup]\nrepo = \""+filepath.ToSlash(repo)+"\"\n"+
			"[storage]\npack_interval = \"75ms\"\npack_max_bytes = 1048576\n"+
			"[[watch]]\nname = \"agent-sessions\"\nsource = \""+filepath.ToSlash(source)+"\"\n"+
			"destination = \"/archives/agents\"\nsettle_time = \"20ms\"\n"+
			"minimum_age = \"24h\"\nscan_interval = \"10ms\"\n",
	), 0o600))
	t.Setenv("DOCBANK_HOME", home)
	t.Setenv(client.EnvBackgroundDaemon, "1")
	startTestDaemon(t, home)

	virtualPath := "/archives/agents/codex/project-alpha/2026-07/session-01.jsonl"
	require.Eventually(t, func() bool {
		out, err := runCLI(t, "cat", virtualPath)
		return err == nil && out == string(first)
	}, 5*time.Second, 25*time.Millisecond)

	out, err := runCLI(t, "versions", "list", virtualPath, "--json")
	require.NoError(t, err)
	var firstVersions api.ContentVersionPage
	require.NoError(t, json.Unmarshal([]byte(out), &firstVersions))
	require.Len(t, firstVersions.Items, 1)
	firstVersionID := firstVersions.Items[0].ID

	writeClosedSession(final)
	require.Eventually(t, func() bool {
		out, catErr := runCLI(t, "cat", virtualPath)
		return catErr == nil && out == string(final)
	}, 5*time.Second, 25*time.Millisecond)

	out, err = runCLI(t, "versions", "list", virtualPath, "--json")
	require.NoError(t, err)
	var versions api.ContentVersionPage
	require.NoError(t, json.Unmarshal([]byte(out), &versions))
	require.Len(t, versions.Items, 2)
	assert.Equal(t, firstVersionID, versions.Items[1].ID)

	require.Eventually(t, func() bool {
		out, searchErr := runCLI(t, "search", "quasar", "--json")
		if searchErr != nil {
			return false
		}
		var report api.SearchReport
		return json.Unmarshal([]byte(out), &report) == nil && len(report.Hits) == 1 &&
			report.Hits[0].Path == virtualPath && report.Hits[0].Match == "content"
	}, 5*time.Second, 100*time.Millisecond)

	out, err = runCLI(t, "provenance", virtualPath, "--json")
	require.NoError(t, err)
	var provenance api.ProvenancePage
	require.NoError(t, json.Unmarshal([]byte(out), &provenance))
	require.Len(t, provenance.Items, 1)
	assert.Equal(t, "watch", provenance.Items[0].SourceKind)
	assert.Equal(t, "agent-sessions", provenance.Items[0].SourceDescription)
	assert.Equal(t, filepath.ToSlash(relativeSource), provenance.Items[0].OriginalPath)
	require.NotNil(t, provenance.Items[0].OriginalMTime)
	assert.Equal(t, oldMTime.Format(time.RFC3339Nano), *provenance.Items[0].OriginalMTime)

	require.Eventually(t, func() bool {
		out, statusErr := runCLI(t, "storage", "status", "--json")
		if statusErr != nil {
			return false
		}
		var status api.StorageStatus
		return json.Unmarshal([]byte(out), &status) == nil &&
			status.LooseBlobs == 0 && status.PackedBlobs == 2
	}, 5*time.Second, 25*time.Millisecond)

	_, err = runCLI(t, "backup", "init")
	require.NoError(t, err)
	var snapshot api.BackupSnapshot
	require.Eventually(t, func() bool {
		out, createErr := runCLI(t, "backup", "create", "--tag", "agent-sessions", "--json")
		if errors.Is(createErr, client.ErrMaintenanceBusy) {
			return false
		}
		if createErr != nil {
			err = createErr
			return true
		}
		err = json.Unmarshal([]byte(out), &snapshot)
		return true
	}, 5*time.Second, 25*time.Millisecond)
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.ID)

	out, err = runCLI(t, "backup", "verify", snapshot.ID, "--json")
	require.NoError(t, err)
	var backupProof api.BackupVerifyReport
	require.NoError(t, json.Unmarshal([]byte(out), &backupProof))
	assert.Empty(t, backupProof.Problems)

	restoreTarget := filepath.Join(t.TempDir(), "restored")
	out, err = runCLI(t, "backup", "restore", snapshot.ID, "--target", restoreTarget, "--json")
	require.NoError(t, err)
	var restoreProof api.BackupRestoreReport
	require.NoError(t, json.Unmarshal([]byte(out), &restoreProof))
	assert.True(t, restoreProof.Proof.ContentVerified)
	assert.True(t, restoreProof.Proof.SQLiteIntegrity)
	assert.True(t, restoreProof.Proof.ManifestStats)

	t.Setenv("DOCBANK_HOME", restoreTarget)
	startTestDaemon(t, restoreTarget)
	out, err = runCLI(t, "cat", virtualPath)
	require.NoError(t, err)
	assert.Equal(t, string(final), out)
	out, err = runCLI(t, "versions", "cat", firstVersionID)
	require.NoError(t, err)
	assert.Equal(t, string(first), out)

	out, err = runCLI(t, "versions", "list", virtualPath, "--json")
	require.NoError(t, err)
	var restoredVersions api.ContentVersionPage
	require.NoError(t, json.Unmarshal([]byte(out), &restoredVersions))
	assert.Equal(t, versions, restoredVersions)

	out, err = runCLI(t, "provenance", virtualPath, "--json")
	require.NoError(t, err)
	var restoredProvenance api.ProvenancePage
	require.NoError(t, json.Unmarshal([]byte(out), &restoredProvenance))
	assert.Equal(t, provenance, restoredProvenance)

	out, err = runCLI(t, "verify")
	require.NoError(t, err)
	assert.Contains(t, out, "2 blob(s) ok")

	sourceBytes, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	assert.Equal(t, final, sourceBytes)
}
