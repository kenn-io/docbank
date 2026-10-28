package misslog

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
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
	defer func() { _ = st.Close() }()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := config.Default().MissLog
	disabled := New(st, cfg, logger, func(_ context.Context, fn func() error) error { return fn() })
	require.Empty(t, disabled.Record(t.Context(), Receipt{Query: "synthetic secret"}))
	cfg.Enabled = true
	svc := New(st, cfg, logger, func(_ context.Context, fn func() error) error { return fn() })
	id := svc.Record(t.Context(), Receipt{Query: "synthetic secret", Scope: map[string]any{"empty": true}})
	require.NotEmpty(t, id)
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NoError(t, st.Close())
	require.Empty(t, svc.Record(context.Background(), Receipt{Query: "another private secret"}))
	require.NotContains(t, logs.String(), "secret")
	require.Contains(t, logs.String(), "miss_log_write_failed")
}

func TestEnabledStartupPrunesButDisabledPreserves(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
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

func TestReceiptSizeAndOutcomes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default().MissLog
	cfg.Enabled = true
	svc := New(st, cfg, nil, func(_ context.Context, fn func() error) error { return fn() })
	ids := make([]string, 4096)
	for i := range ids {
		ids[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}
	scope := ReceiptScope(map[string]any{"query": "missing", "profile": "private", "fence": map[string]any{"content_version_ids": ids}, "media_sources": ids})
	id := svc.Record(t.Context(), Receipt{Query: "missing", Scope: scope, FenceFingerprint: "sha256:" + strings.Repeat("a", 64),
		Truncated: true, Degradations: []string{"reranking_degraded"}, Reranking: map[string]any{"outcome": "degraded", "cause": "timeout", "candidate_count": 0}})
	require.NotEmpty(t, id)
	row, err := st.MissLogRecordByID(t.Context(), id)
	require.NoError(t, err)
	require.Less(t, len(row.Receipt), 3<<10)
	var receipt Receipt
	require.NoError(t, json.Unmarshal([]byte(row.Receipt), &receipt))
	require.Equal(t, "sha256:"+strings.Repeat("a", 64), receipt.FenceFingerprint)
	require.NotContains(t, receipt.Scope, "fence")
	require.NotContains(t, receipt.Scope, "media_sources")
	require.True(t, receipt.Truncated)
	require.Equal(t, []string{"reranking_degraded"}, receipt.Degradations)
	require.Equal(t, "degraded", receipt.Reranking["outcome"])
	require.Equal(t, "timeout", receipt.Reranking["cause"])
	require.Empty(t, svc.Record(t.Context(), Receipt{Query: strings.Repeat("x", 8<<20)}))
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
}
