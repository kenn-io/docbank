package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/report"
)

func TestReportPDFRestore(t *testing.T) {
	f := newPDFReportFixture(t)
	var configuration bytes.Buffer
	require.NoError(t, toml.NewEncoder(&configuration).Encode(map[string]any{
		"backup": map[string]string{"repo": filepath.Join(t.TempDir(), "backup")},
	}))
	configPath := filepath.Join(f.root, "config.toml")
	require.NoError(t, os.WriteFile(configPath, configuration.Bytes(), 0o600))
	stop := f.start(t, f.root)
	summary, packet := f.capture(t, f.request)
	archive, receipt := exportReportPDFs(t, f.originals[:3])
	history := pdfReportHistory(t)
	require.Len(t, history.Items, 2)
	for _, item := range history.Items {
		require.Equal(t, f.request, item.Request, "history must retain the request without date choices")
	}
	_, err := runCLI(t, "backup", "init")
	require.NoError(t, err)
	current := backupPDFSnapshot(t, "current-selection")

	replacement := reportPDF(t, "Replacement without the selected terms or dated evidence.")
	_, err = runCLI(t, "put", writeSourceFile(t, "replacement.pdf", string(replacement)),
		"/alpha.pdf", "--progress", "plain")
	require.NoError(t, err)
	_, err = runCLI(t, "rm", "/missing.pdf")
	require.NoError(t, err)
	require.Equal(t, summary, showPDFReport(t, summary.ID))
	frozen := filepath.Join(t.TempDir(), "frozen.zip")
	_, err = runCLI(t, "search-export", "download", summary.ID, "--output", frozen)
	require.NoError(t, err)
	want, err := os.ReadFile(packet)
	require.NoError(t, err)
	actual, err := os.ReadFile(frozen)
	require.NoError(t, err)
	require.Equal(t, want, actual, "replacement and trash must leave the captured report unchanged")
	assertPDFSelectionChanged(t, f.request)
	changed := backupPDFSnapshot(t, "changed-selection")
	targets := []struct{ name, root string }{
		{"current-selection", restorePDFSnapshot(t, current)},
		{"changed-selection", restorePDFSnapshot(t, changed)},
	}
	// Both restores use the source daemon. Stop it before changing the CLI home.
	stop()
	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			stopRestored := f.start(t, target.root)
			defer stopRestored()
			restoredHistory := pdfReportHistory(t)
			require.Equal(t, history, restoredHistory)
			var request report.Request
			for _, item := range restoredHistory.Items {
				if item.Summary.ID == summary.ID {
					request = item.Request
				}
			}
			require.Equal(t, f.request, request)
			require.Empty(t, request.DateChoices)
			_, err := runCLI(t, "search-export", "show", summary.ID, "--json")
			requireReportProblem(t, err, "report_unavailable")
			missing := filepath.Join(t.TempDir(), "old-report.zip")
			_, err = runCLI(t, "search-export", "download", summary.ID, "--output", missing)
			requireReportProblem(t, err, "report_unavailable")
			require.NoFileExists(t, missing)
			originals := f.originals[:3]
			if target.name == "current-selection" {
				fresh, _ := f.capture(t, request)
				require.NotEqual(t, summary.ID, fresh.ID)
				require.True(t, fresh.ObservedAt.After(summary.ObservedAt))
			} else {
				assertPDFSelectionChanged(t, request)
				// Historical alpha is retained; missing.pdf remains trashed.
				// The original member deliberately has revision zero.
				originals = f.originals[:1]
			}
			path, proof := exportReportPDFs(t, originals)
			assertPDFArchive(t, path, proof, originals)
		})
	}
	// These checks use only the previously saved files, after all daemons stop.
	out, err := runCLI(t, "search-export", "verify", packet)
	require.NoError(t, err)
	require.Contains(t, out, "internally consistent: true; source verified: false")
	assertPDFArchive(t, archive, receipt, f.originals[:3])
}

func assertPDFSelectionChanged(t *testing.T, request report.Request) {
	t.Helper()
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	destination := filepath.Join(t.TempDir(), "stale.zip")
	_, err = runCLI(t, "search-export", "create", "--input",
		writeSourceFile(t, "stale.json", string(encoded)), "--output", destination)
	requireReportProblem(t, err, "report_selection_changed")
	require.NoFileExists(t, destination)
}

func backupPDFSnapshot(t *testing.T, tag string) string {
	t.Helper()
	out, err := runCLI(t, "backup", "create", "--tag", tag, "--jobs", "1", "--json")
	require.NoError(t, err)
	var snapshot api.BackupSnapshot
	require.NoError(t, json.Unmarshal([]byte(out), &snapshot))
	out, err = runCLI(t, "backup", "verify", snapshot.ID, "--json")
	require.NoError(t, err)
	var proof api.BackupVerifyReport
	require.NoError(t, json.Unmarshal([]byte(out), &proof))
	require.Empty(t, proof.Problems)
	require.Equal(t, []string{snapshot.ID}, proof.Snapshots)
	return snapshot.ID
}

func restorePDFSnapshot(t *testing.T, snapshot string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "restored")
	out, err := runCLI(t, "backup", "restore", snapshot, "--target", target,
		"--jobs", "1", "--json")
	require.NoError(t, err)
	var proof api.BackupRestoreReport
	require.NoError(t, json.Unmarshal([]byte(out), &proof))
	require.Equal(t, snapshot, proof.SnapshotID)
	require.True(t, proof.Proof.ContentVerified)
	require.True(t, proof.Proof.SQLiteIntegrity)
	require.True(t, proof.Proof.ManifestStats)
	require.NotNil(t, proof.Storage)
	require.Empty(t, proof.StorageWarning)
	return target
}
