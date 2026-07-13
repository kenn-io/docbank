package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/backupapp"
)

func decodeBackupEvents(t *testing.T, body string) []api.BackupCreateEvent {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(body))
	var events []api.BackupCreateEvent
	for {
		var event api.BackupCreateEvent
		err := decoder.Decode(&event)
		if err == io.EOF {
			return events
		}
		require.NoError(t, err)
		events = append(events, event)
	}
}

func TestBackupInitCreateListRoundTrip(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "repo")
	ts, s := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = repoPath })
	createFileWithContent(t, ts, s, "/contract.txt", "backup through the daemon")

	resp, body := do(t, ts, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var repository api.BackupRepository
	require.NoError(t, json.Unmarshal([]byte(body), &repository))
	assert.NotEmpty(t, repository.ID)
	assert.Equal(t, repoPath, repository.Path)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/snapshots", nil,
		map[string]any{"tag": "first", "jobs": 1})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var snapshot api.BackupSnapshot
	require.NoError(t, json.Unmarshal([]byte(body), &snapshot))
	assert.NotEmpty(t, snapshot.ID)
	assert.Empty(t, snapshot.ParentID)
	assert.Equal(t, "first", snapshot.Tag)
	assert.Equal(t, backupapp.MetadataFormat, snapshot.MetadataFormat)
	assert.Equal(t, int64(1), snapshot.Files)
	assert.Equal(t, int64(1), snapshot.Blobs)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/snapshots", nil,
		map[string]any{"tag": "second", "jobs": 1})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var second api.BackupSnapshot
	require.NoError(t, json.Unmarshal([]byte(body), &second))
	assert.Equal(t, snapshot.ID, second.ParentID)

	resp, body = get(t, ts, "/api/v1/backup/snapshots", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var listed api.BackupSnapshotList
	require.NoError(t, json.Unmarshal([]byte(body), &listed))
	require.Len(t, listed.Items, 2)
	assert.Equal(t, snapshot.ID, listed.Items[0].ID)
	assert.Equal(t, second.ID, listed.Items[1].ID)

	repo, err := backup.Open(repoPath)
	require.NoError(t, err)
	verified, err := backup.Verify(t.Context(), repo, backupapp.New("test"), backup.VerifyOptions{})
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)
}

func TestBackupRoutesValidateRepositoryAndReportLock(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/api/v1/backup/snapshots", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"backup_repository"`)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/init", nil,
		map[string]any{"repo": "relative/repo"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"validation"`)

	repoPath := filepath.Join(t.TempDir(), "repo")
	resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/init", nil,
		map[string]any{"repo": repoPath})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	repo, err := backup.Open(repoPath)
	require.NoError(t, err)
	lock, err := repo.AcquireExclusiveLock("test", false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })

	resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/snapshots", nil,
		map[string]any{"repo": repoPath})
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, body, `"code":"backup_locked"`)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/snapshots/stream", nil,
		map[string]any{"repo": repoPath})
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"streaming errors are terminal events after response headers commit")
	events := decodeBackupEvents(t, body)
	require.Len(t, events, 1)
	require.NotNil(t, events[0].Error)
	assert.Equal(t, "error", events[0].Type)
	assert.Equal(t, "backup_locked", events[0].Error.Code)
}

func TestBackupCreateProgressStreamEndsWithResult(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "repo")
	ts, s := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = repoPath })
	createFileWithContent(t, ts, s, "/stream.txt", "visible work")

	resp, body := do(t, ts, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/snapshots/stream", nil,
		map[string]any{"tag": "streamed", "jobs": 1})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/x-ndjson")
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))

	events := decodeBackupEvents(t, body)
	require.NotEmpty(t, events)
	finalStages := map[string]bool{}
	for i, event := range events[:len(events)-1] {
		assert.Equal(t, "progress", event.Type, "event %d", i)
		require.NotNil(t, event.Progress)
		if event.Progress.Final {
			finalStages[event.Progress.Stage] = true
		}
	}
	for _, stage := range []string{"freeze", "metadata", "attachments", "seal"} {
		assert.True(t, finalStages[stage], "stage %q emitted a final event", stage)
	}
	terminal := events[len(events)-1]
	assert.Equal(t, "result", terminal.Type)
	require.NotNil(t, terminal.Snapshot)
	assert.Equal(t, "streamed", terminal.Snapshot.Tag)
	assert.Equal(t, int64(1), terminal.Snapshot.Files)
}
