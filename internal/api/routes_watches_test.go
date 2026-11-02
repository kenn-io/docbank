package api_test

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/sqlite"
)

func TestListWatchedInboxesReturnsEffectiveConfigAndRunnerState(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	supervisor := jobs.New(ctx, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.Background())) })
	running := make(chan struct{})
	release := make(chan struct{})
	require.NoError(t, supervisor.Start("watch:archive", func(ctx context.Context) error {
		close(running)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	<-running

	source := filepath.Join(t.TempDir(), "source")
	ts, st := newTestServer(t, func(d *api.Deps) {
		d.Jobs = supervisor
		d.Cfg.Watches = []config.WatchConfig{
			{
				Name: "zeta", Source: filepath.Join(t.TempDir(), "later"),
				Destination: "/later", SettleTime: config.Duration(time.Minute),
				ScanInterval: config.Duration(time.Second),
			},
			{
				Name: "archive", Source: source, Destination: "/records", Tags: []string{"receipts"},
				CaptureOnChange: true,
				SettleTime:      config.Duration(2 * time.Minute),
				MinimumAge:      config.Duration(7 * 24 * time.Hour),
				ScanInterval:    config.Duration(15 * time.Second),
				Exclude:         []string{"cache/", ".DS_Store"},
				ExcludePatterns: []string{"*.swp", ".syncthing.*.tmp"},
			},
		}
	})
	resp, body := get(t, ts, "/api/v1/watches", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got api.WatchedInboxList
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Len(t, got.Items, 2)
	assert.Equal(t, "archive", got.Items[0].Name)
	assert.Equal(t, []string{"receipts"}, got.Items[0].Tags)
	assert.Equal(t, `tag "receipts" is unavailable`, got.Items[0].TagError)
	assert.True(t, got.Items[0].CaptureOnChange)
	assert.Contains(t, body, `"capture_on_change":true`)
	assert.Equal(t, source, got.Items[0].Source)
	assert.Equal(t, "/records", got.Items[0].Destination)
	assert.Equal(t, "2m0s", got.Items[0].SettleTime)
	assert.Equal(t, "168h0m0s", got.Items[0].MinimumAge)
	assert.Equal(t, "15s", got.Items[0].ScanInterval)
	assert.Equal(t, []string{"cache/", ".DS_Store"}, got.Items[0].Exclude)
	assert.Equal(t, []string{"*.swp", ".syncthing.*.tmp"}, got.Items[0].ExcludePatterns)
	assert.Empty(t, got.Items[1].ExcludePatterns)
	assert.Contains(t, body, `"exclude_patterns":[]`)
	require.NotNil(t, got.Items[0].Job)
	assert.Equal(t, "watch:archive", got.Items[0].Job.Name)
	assert.Equal(t, "running", got.Items[0].Job.Status)
	assert.Equal(t, "zeta", got.Items[1].Name)
	assert.False(t, got.Items[1].CaptureOnChange)
	assert.Equal(t, "0s", got.Items[1].MinimumAge)
	assert.Empty(t, got.Items[1].Exclude)
	assert.Nil(t, got.Items[1].Job)
	_, err := st.CreateTag(t.Context(), "receipts")
	require.NoError(t, err)
	resp, body = get(t, ts, "/api/v1/watches", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Len(t, got.Items, 2)
	assert.Empty(t, got.Items[0].TagError)
	close(release)
}

func TestListWatchedInboxesReturnsEmptyObject(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/api/v1/watches", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got api.WatchedInboxList
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Empty(t, got.Items)
}

func TestWatchProcessingStatusLookupFailure(t *testing.T) {
	t.Parallel()
	supervisor := jobs.New(t.Context(), slog.New(slog.DiscardHandler))
	t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.Background())) })
	require.NoError(t, supervisor.Start("watch:inbox", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}))
	ts, catalog := newTestServer(t, func(d *api.Deps) {
		d.Jobs = supervisor
		d.Cfg.Watches = []config.WatchConfig{{Name: "inbox", Profile: "private"}}
		configureProcessingTestService(t)(d)
		t.Cleanup(func() { require.NoError(t, d.Processing.Shutdown(context.Background())) })
	})
	db, err := catalog.SQLiteDriver().Open(catalog.DBPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	// Break only consent lookup so the shared catalog can still list jobs.
	_, err = db.ExecContext(t.Context(), `DROP TABLE processing_consent_grants`)
	require.NoError(t, err)
	resp, body := get(t, ts, "/api/v1/watches", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var watches api.WatchedInboxList
	require.NoError(t, json.Unmarshal([]byte(body), &watches))
	require.Len(t, watches.Items, 1)
	require.NotNil(t, watches.Items[0].Processing)
	assert.Contains(t, watches.Items[0].Processing.Error, "Consent status unavailable")
	resp, body = get(t, ts, "/api/v1/jobs", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var jobs api.JobList
	require.NoError(t, json.Unmarshal([]byte(body), &jobs))
	require.Len(t, jobs.Items, 1)
	assert.Equal(t, watches.Items[0].Processing, jobs.Items[0].Processing)
	assert.Empty(t, jobs.Items[0].Error, "consent lookup failures leave the scanner healthy")
}
