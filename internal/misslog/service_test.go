package misslog

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestRecordingIsOptInBoundedAndSanitizesFailures(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := config.Default().MissLog
	mutate := func(_ context.Context, fn func() error) error { return fn() }
	disabled := New(st, cfg, logger, mutate)
	disabled.Record(t.Context(), Request{Query: "synthetic secret"})
	cfg.Enabled = true
	svc := New(st, cfg, logger, mutate)
	svc.Record(t.Context(), Request{Query: " \n\t"})
	svc.Record(t.Context(), Request{Query: "oversized", Filters: map[string]any{"filter": strings.Repeat("x", 64<<10)}})
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
	svc.Record(t.Context(), Request{Query: "synthetic secret", RequestedMode: "hybrid", ActualMode: "lexical", DocumentCount: new(0),
		FenceFingerprint: "sha256:synthetic", Coverage: map[string]any{"state": "complete"}, Truncated: true,
		Degradations: []string{"reranking_degraded"}, Reranking: map[string]any{"outcome": "degraded"}})
	rows, err = st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var receipt Receipt
	require.NoError(t, json.Unmarshal([]byte(rows[0].Receipt), &receipt))
	require.Equal(t, rows[0].ID, receipt.ID)
	require.Equal(t, st.VaultID(), receipt.VaultUID)
	require.WithinDuration(t, time.Now(), receipt.CreatedAt, time.Second)
	require.Equal(t, "synthetic secret", receipt.Query)
	require.Equal(t, "hybrid", receipt.RequestedMode)
	require.Equal(t, "lexical", receipt.ActualMode)
	require.Equal(t, new(0), receipt.DocumentCount)
	require.Equal(t, "sha256:synthetic", receipt.FenceFingerprint)
	require.True(t, receipt.Truncated)
	require.Equal(t, "complete", receipt.Coverage["state"])
	require.Equal(t, []string{"reranking_degraded"}, receipt.Degradations)
	require.Equal(t, "degraded", receipt.Reranking["outcome"])
	require.Equal(t, 1, strings.Count(rows[0].Receipt, "synthetic secret"))
	require.NoError(t, st.Close())
	svc.Record(context.Background(), Request{Query: "another private secret"})
	require.NotContains(t, logs.String(), "secret")
	require.Contains(t, logs.String(), "miss_log_write_failed")
}

func TestEnabledStartupPrunesButDisabledPreserves(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	require.NoError(t, st.AddMissLogRecord(t.Context(), store.MissLogRecord{ID: "expired", CreatedAt: now.AddDate(0, 0, -91), Receipt: `{}`}, now, 365, 5000))
	cfg := config.Default().MissLog
	New(st, cfg, nil, func(_ context.Context, fn func() error) error { return fn() })
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	cfg.Enabled = true
	New(st, cfg, nil, func(_ context.Context, fn func() error) error { return fn() })
	rows, err = st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}
