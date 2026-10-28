package misslog

import (
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

func TestRecordingAndEnabledStartupPruning(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default().MissLog
	mutate := func(_ context.Context, fn func() error) error { return fn() }
	cfg.Enabled = true
	svc := New(st, cfg, slog.Default(), mutate)
	svc.Record(t.Context(), Request{Query: "synthetic secret", RequestedMode: "hybrid", ActualMode: "lexical", DocumentCount: new(0),
		FenceFingerprint: "sha256:synthetic", Coverage: map[string]any{"state": "complete"}, Truncated: true,
		Degradations: []string{"reranking_degraded"}, Reranking: map[string]any{"outcome": "degraded"}})
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var receipt Receipt
	require.NoError(t, json.Unmarshal([]byte(rows[0].Receipt), &receipt))
	require.Equal(t, "synthetic secret", receipt.Query)
	require.Equal(t, []string{"hybrid", "lexical"}, []string{receipt.RequestedMode, receipt.ActualMode})
	require.Equal(t, []string{"reranking_degraded"}, receipt.Degradations)
	require.Equal(t, 1, strings.Count(rows[0].Receipt, "synthetic secret"))
	now := time.Now()
	require.NoError(t, st.AddMissLogRecord(t.Context(), store.MissLogRecord{ID: "expired", CreatedAt: now.AddDate(0, 0, -91), Receipt: `{}`}, now, 365, 5000))
	cfg.Enabled = false
	New(st, cfg, slog.Default(), func(_ context.Context, fn func() error) error { return fn() })
	rows, err = st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	cfg.Enabled = true
	New(st, cfg, slog.Default(), func(_ context.Context, fn func() error) error { return fn() })
	rows, err = st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
}
