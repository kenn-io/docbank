package api_test

import (
	"context"
	"encoding/json"
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
)

func TestListWatchedInboxesReturnsEffectiveConfigAndRunnerState(t *testing.T) {
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
	ts, _ := newTestServer(t, func(d *api.Deps) {
		d.Jobs = supervisor
		d.Cfg.Watches = []config.WatchConfig{
			{
				Name: "zeta", Source: filepath.Join(t.TempDir(), "later"),
				Destination: "/later", SettleTime: config.Duration(time.Minute),
				ScanInterval: config.Duration(time.Second),
			},
			{
				Name: "archive", Source: source, Destination: "/records",
				SettleTime:   config.Duration(2 * time.Minute),
				ScanInterval: config.Duration(15 * time.Second),
				Exclude:      []string{"cache/", ".DS_Store"},
			},
		}
	})
	resp, body := get(t, ts, "/api/v1/watches", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got api.WatchedInboxList
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Len(t, got.Items, 2)
	assert.Equal(t, "archive", got.Items[0].Name)
	assert.Equal(t, source, got.Items[0].Source)
	assert.Equal(t, "/records", got.Items[0].Destination)
	assert.Equal(t, "2m0s", got.Items[0].SettleTime)
	assert.Equal(t, "15s", got.Items[0].ScanInterval)
	assert.Equal(t, []string{"cache/", ".DS_Store"}, got.Items[0].Exclude)
	require.NotNil(t, got.Items[0].Job)
	assert.Equal(t, "watch:archive", got.Items[0].Job.Name)
	assert.Equal(t, "running", got.Items[0].Job.Status)
	assert.Equal(t, "zeta", got.Items[1].Name)
	assert.Empty(t, got.Items[1].Exclude)
	assert.Nil(t, got.Items[1].Job)
	close(release)
}

func TestListWatchedInboxesReturnsEmptyObject(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/api/v1/watches", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got api.WatchedInboxList
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Empty(t, got.Items)
}
