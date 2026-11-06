package misslog

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/kit/atomicfile"
)

func TestRecordingIsOptInAndBounded(t *testing.T) {
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
	filters := map[string]any{"tag_id": "taxes", "asset_ids": []string{"synthetic-photo"}, "set_ids": []string{"synthetic-set"}}
	svc.Record(t.Context(), Request{RequestedMode: "lexical", ActualMode: "lexical", Filters: filters})
	nestedFilters := map[string]any{"asset_ids": []string{"synthetic-document"}, "tag_ids": []string{"synthetic-tag"}}
	svc.Record(t.Context(), Request{Query: "synthetic secret", RequestedMode: "hybrid", ActualMode: "lexical", Filters: map[string]any{"syntax": "literal", "filters": nestedFilters}, DocumentCount: new(0), FenceFingerprint: "sha256:synthetic", Coverage: map[string]any{"state": "complete"}, Truncated: true, Degradations: []string{"reranking_degraded"}, Reranking: map[string]any{"outcome": "degraded"}})
	require.Equal(t, []string{"synthetic-photo"}, filters["asset_ids"])
	require.Equal(t, []string{"synthetic-document"}, nestedFilters["asset_ids"])
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
	require.Equal(t, map[string]any{"syntax": "literal", "filters": map[string]any{"tag_ids": []any{"synthetic-tag"}}}, receipt.Filters)
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
	require.Equal(t, map[string]any{"tag_id": "taxes", "set_ids": []any{"synthetic-set"}}, rows[0].Filters)
	require.NoError(t, reopened.Purge(t.Context()))
	require.ErrorIs(t, reopened.Delete(t.Context(), receipt.ID), os.ErrNotExist)
	entries, err := os.ReadDir(filepath.Join(root, "miss-log"))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestRecordingSanitizesDirectoryFailures(t *testing.T) {
	root := t.TempDir()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := config.Default().MissLog
	cfg.Enabled = true
	svc := New(t.Context(), root, "synthetic-vault", cfg, logger)
	require.NoError(t, os.WriteFile(filepath.Join(root, "miss-log"), []byte("blocked"), 0600))
	svc.Record(t.Context(), Request{Query: "another private secret"})
	require.NotContains(t, logs.String(), "secret")
	require.Contains(t, logs.String(), "miss_log_write_failed")
}

type purgeContext struct {
	context.Context

	beforeErr func()
}

func (ctx purgeContext) Err() error {
	ctx.beforeErr()
	return ctx.Context.Err()
}

func TestPurgeRemovesUnindexedFiles(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default().MissLog
	cfg.Enabled = true
	svc := New(t.Context(), root, "synthetic-vault", cfg, nil)
	svc.Record(t.Context(), Request{Query: "indexed secret"})
	svc.Record(t.Context(), Request{Query: "another indexed secret"})
	rows, err := svc.List(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	survivor := rows[0]
	dir := filepath.Join(root, "miss-log")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	removedPath := filepath.Join(dir, entries[0].Name())
	for _, name := range []string{".receipt.json.tmp-interrupted", "unindexed.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("synthetic query bytes"), 0600))
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Cancel after an indexed receipt is deleted, with another still on disk.
	err = svc.Purge(purgeContext{Context: ctx, beforeErr: func() {
		if _, err := os.Stat(removedPath); errors.Is(err, os.ErrNotExist) {
			cancel()
		}
	}})
	require.ErrorIs(t, err, context.Canceled)
	rows, err = svc.List(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Equal(t, []Receipt{survivor}, rows)
	require.NoError(t, svc.Purge(t.Context()))
	entries, err = os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
	rows, err = svc.List(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestDisabledRecordingStillPrunesAndExpires(t *testing.T) {
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
		cfg.Enabled, cfg.MaxRecords = false, 1
		disabled := New(t.Context(), root, "synthetic-vault", cfg, nil)
		rows, err = disabled.List(t.Context(), 10, 0)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, "third", rows[0].Query)
		entries, err := os.ReadDir(filepath.Join(root, "miss-log"))
		require.NoError(t, err)
		require.Len(t, entries, 1)
		time.Sleep(91 * 24 * time.Hour)
		rows, err = disabled.List(t.Context(), 10, 0)
		require.NoError(t, err)
		require.Empty(t, rows)
		disabled = New(t.Context(), root, "synthetic-vault", cfg, nil)
		rows, err = disabled.List(t.Context(), 10, 0)
		require.NoError(t, err)
		require.Empty(t, rows)
		entries, err = os.ReadDir(filepath.Join(root, "miss-log"))
		require.NoError(t, err)
		require.Empty(t, entries)
	})
}

func TestCancelledPublishedWriteStillPrunes(t *testing.T) {
	for _, publishedErr := range []error{nil, errors.Join(atomicfile.ErrNotDurable, errors.New("directory sync failed"))} {
		t.Run(map[bool]string{true: "durable", false: "directory sync failed"}[publishedErr == nil], func(t *testing.T) {
			root := t.TempDir()
			cfg := config.Default().MissLog
			cfg.Enabled, cfg.MaxRecords = true, 1
			svc := New(t.Context(), root, "synthetic-vault", cfg, nil)
			svc.Record(t.Context(), Request{Query: "first"})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			svc.writeFile = func(path string, data []byte, opts ...atomicfile.Option) error {
				if err := atomicfile.WriteFile(path, data, opts...); err != nil {
					return err
				}
				cancel()
				return publishedErr
			}
			svc.Record(ctx, Request{Query: "second"})
			entries, err := os.ReadDir(filepath.Join(root, "miss-log"))
			require.NoError(t, err)
			require.Len(t, entries, 1)
			rows, err := svc.List(t.Context(), 10, 0)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, "second", rows[0].Query)
		})
	}
}

func TestZeroBoundsUseDefaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc := New(t.Context(), t.TempDir(), "synthetic-vault", config.MissLogConfig{Enabled: true}, nil)
		svc.Record(t.Context(), Request{Query: "first"})
		svc.Record(t.Context(), Request{Query: "second"})
		time.Sleep(89 * 24 * time.Hour)
		rows, err := svc.List(t.Context(), 10, 0)
		require.NoError(t, err)
		require.Len(t, rows, 2)
		time.Sleep(2 * 24 * time.Hour)
		rows, err = svc.List(t.Context(), 10, 0)
		require.NoError(t, err)
		require.Empty(t, rows)
	})
}
