package api_test

import (
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func TestBackupScheduleAPI(t *testing.T) {
	t.Parallel()
	wake := make(chan struct{}, 1)
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Backup.Repo = filepath.Join(t.TempDir(), "repo")
		d.Cfg.Backup.KeepLast = 5
		d.BackupScheduleWake = wake
		// A closed supervisor must not fail a schedule write.
		d.Jobs = jobs.New(t.Context(), slog.New(slog.DiscardHandler))
		require.NoError(t, d.Jobs.Shutdown(t.Context()))
	})
	_, err := s.SetPhotoSettings(t.Context(), 1, store.PhotoSettingsUpdate{Preference: new("image")})
	require.NoError(t, err)
	schedule := func(body string) api.BackupSchedule {
		t.Helper()
		var out api.BackupSchedule
		require.NoError(t, json.Unmarshal([]byte(body), &out))
		return out
	}
	res, body := do(t, ts, http.MethodGet, "/api/v1/backup/schedule", nil, nil)
	require.Equal(t, http.StatusOK, res.StatusCode, body)
	require.Equal(t, `"2"`, res.Header.Get("ETag"))
	require.Equal(t, api.BackupSchedule{EffectiveKeepLast: 5, Revision: 2}, schedule(body))

	headers := map[string]string{"If-Match": `"2"`}
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts), "If-Match": `"2"`}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		res, body = do(t, ts, method, "/api/v1/backup/schedule", browser, map[string]any{"keep_last": 2})
		require.Equal(t, http.StatusForbidden, res.StatusCode, body)
	}
	res, body = do(t, ts, http.MethodPut, "/api/v1/backup/schedule", nil, map[string]any{"keep_last": 2})
	require.Equal(t, http.StatusPreconditionRequired, res.StatusCode, body)
	for _, request := range []map[string]any{{}, {"interval_seconds": nil}, {"keep_last": 2, "reset_keep_last": true}, {"repo": "/synthetic"}} {
		res, body = do(t, ts, http.MethodPut, "/api/v1/backup/schedule", headers, request)
		require.Equal(t, http.StatusUnprocessableEntity, res.StatusCode, body)
	}
	for _, tc := range []struct {
		seconds int
		detail  string
	}{
		{seconds: -3600},
		{seconds: 30, detail: "at least one minute"},
	} {
		res, body = do(t, ts, http.MethodPut, "/api/v1/backup/schedule", headers, map[string]any{"interval_seconds": tc.seconds})
		require.Equal(t, http.StatusUnprocessableEntity, res.StatusCode, body)
		if tc.detail != "" {
			problem := decodeProblem(t, body)
			require.Equal(t, "validation", problem.Code)
			require.Contains(t, problem.Detail, tc.detail)
		}
	}
	require.Empty(t, wake, "refused writes do not wake the schedule")

	res, body = do(t, ts, http.MethodPut, "/api/v1/backup/schedule", headers, map[string]any{"interval_seconds": 3600, "keep_last": 2})
	require.Equal(t, http.StatusOK, res.StatusCode, body)
	require.Equal(t, api.BackupSchedule{IntervalSeconds: new(int64(3600)), KeepLast: new(2), EffectiveIntervalSeconds: 3600, EffectiveKeepLast: 2, Revision: 3}, schedule(body))
	require.Len(t, wake, 1)
	<-wake
	stale, staleBody := do(t, ts, http.MethodPut, "/api/v1/backup/schedule", headers, map[string]any{"reset_interval": true})
	require.Equal(t, http.StatusPreconditionFailed, stale.StatusCode, staleBody)
	res, body = do(t, ts, http.MethodPut, "/api/v1/backup/schedule", map[string]string{"If-Match": `"3"`}, map[string]any{"reset_interval": true, "reset_keep_last": true})
	require.Equal(t, http.StatusOK, res.StatusCode, body)
	require.Equal(t, api.BackupSchedule{EffectiveKeepLast: 5, Revision: 4}, schedule(body))
	require.Len(t, wake, 1)

	settings, err := s.PhotoSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, new("image"), settings.Preference, "schedule writes keep the display preference")
}

// Without a configured repository an interval override cannot run.
func TestBackupScheduleAPIRequiresConfiguredRepository(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup = config.BackupConfig{KeepLast: 1} })
	res, body := do(t, ts, http.MethodPut, "/api/v1/backup/schedule", map[string]string{"If-Match": `"1"`}, map[string]any{"interval_seconds": 3600})
	require.Equal(t, http.StatusUnprocessableEntity, res.StatusCode, body)
	require.Contains(t, decodeProblem(t, body).Detail, "repo and positive keep_last")
}
