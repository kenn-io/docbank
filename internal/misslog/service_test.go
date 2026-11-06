package misslog

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
)

func TestRecordingIsOptInBoundedAndSanitizesFailures(t *testing.T) {
	root := t.TempDir()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := config.Default().MissLog
	disabled := New(t.Context(), root, "synthetic-vault", cfg, logger)
	disabled.Record(t.Context(), Request{Query: "synthetic secret"})
	rows, err := disabled.List(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
	cfg.Enabled = true
	svc := New(t.Context(), root, "synthetic-vault", cfg, logger)
	svc.Record(t.Context(), Request{Query: " \n\t"})
	svc.Record(t.Context(), Request{Query: "oversized", Filters: map[string]any{"filter": strings.Repeat("x", 64<<10)}})
	svc.Record(t.Context(), Request{RequestedMode: "lexical", ActualMode: "lexical", Filters: map[string]any{"tag_id": "taxes"}})
	svc.Record(t.Context(), Request{Query: "synthetic secret", RequestedMode: "hybrid", ActualMode: "lexical", DocumentCount: new(0), FenceFingerprint: "sha256:synthetic", Coverage: map[string]any{"state": "complete"}, Truncated: true, Degradations: []string{"reranking_degraded"}, Reranking: map[string]any{"outcome": "degraded"}})
	// Reopening observes the persisted receipts, independent of the writer's index.
	reopened := New(t.Context(), root, "synthetic-vault", cfg, logger)
	rows, err = reopened.List(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	var receipt Receipt
	for _, row := range rows {
		if row.Query == "synthetic secret" {
			receipt = row
		}
	}
	require.Equal(t, "synthetic-vault", receipt.VaultUID)
	require.NotEmpty(t, receipt.ID)
	require.Equal(t, "hybrid", receipt.RequestedMode)
	require.Equal(t, "lexical", receipt.ActualMode)
	require.Equal(t, new(0), receipt.DocumentCount)
	require.Equal(t, "sha256:synthetic", receipt.FenceFingerprint)
	require.True(t, receipt.Truncated)
	require.Equal(t, "complete", receipt.Coverage["state"])
	require.Equal(t, []string{"reranking_degraded"}, receipt.Degradations)
	require.Equal(t, "degraded", receipt.Reranking["outcome"])
	require.NoError(t, reopened.Delete(t.Context(), receipt.ID))
	rows, err = reopened.List(t.Context(), 1, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "taxes", rows[0].Filters["tag_id"])
	require.NoError(t, reopened.Purge(t.Context()))
	require.ErrorIs(t, reopened.Delete(t.Context(), receipt.ID), os.ErrNotExist)
	entries, err := os.ReadDir(filepath.Join(root, "miss-log"))
	require.NoError(t, err)
	require.Empty(t, entries)
	require.NoError(t, os.Remove(filepath.Join(root, "miss-log")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "miss-log"), []byte("blocked"), 0600))
	svc.Record(t.Context(), Request{Query: "another private secret"})
	require.NotContains(t, logs.String(), "secret")
	require.Contains(t, logs.String(), "miss_log_write_failed")
}

func TestEnabledStartupPrunesButDisabledPreserves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		cfg := config.Default().MissLog
		cfg.Enabled, cfg.MaxRecords = true, 2
		svc := New(t.Context(), root, "synthetic-vault", cfg, nil)
		for _, query := range []string{"first", "second", "third"} {
			svc.Record(t.Context(), Request{Query: query})
			time.Sleep(time.Second)
		}
		rows, err := svc.List(t.Context(), 1, 1)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, "second", rows[0].Query)
		time.Sleep(91 * 24 * time.Hour)
		rows, err = svc.List(t.Context(), 10, 0)
		require.NoError(t, err)
		require.Empty(t, rows)
		cfg.Enabled = false
		disabled := New(t.Context(), root, "synthetic-vault", cfg, nil)
		rows, err = disabled.List(t.Context(), 10, 0)
		require.NoError(t, err)
		require.Len(t, rows, 2)
		cfg.Enabled = true
		enabled := New(t.Context(), root, "synthetic-vault", cfg, nil)
		rows, err = enabled.List(t.Context(), 10, 0)
		require.NoError(t, err)
		require.Empty(t, rows)
		entries, err := os.ReadDir(filepath.Join(root, "miss-log"))
		require.NoError(t, err)
		require.Empty(t, entries)
	})
}
