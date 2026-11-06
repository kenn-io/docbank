package misslog

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	filters := map[string]any{"tag_id": "taxes", "set_ids": []any{"synthetic-set"}}
	svc.Record(t.Context(), Request{RequestedMode: "lexical", ActualMode: "lexical", Filters: filters})
	nestedFilters := map[string]any{"tag_ids": []any{"synthetic-tag"}}
	svc.Record(t.Context(), Request{Query: "synthetic secret", RequestedMode: "hybrid", ActualMode: "lexical", Filters: map[string]any{"syntax": "literal", "filters": nestedFilters}, DocumentCount: new(0), FenceFingerprint: "sha256:synthetic", Coverage: map[string]any{"state": "complete"}, Truncated: true, Degradations: []string{"reranking_degraded"}, Reranking: map[string]any{"outcome": "degraded"}})
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
	require.Equal(t, 1, receipt.Version)
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

func TestMissLogMissingReceipt(t *testing.T) {
	for _, operation := range []string{"list", "delete"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := t.TempDir()
				cfg := config.Default().MissLog
				cfg.Enabled = true
				svc := New(t.Context(), root, "synthetic-vault", cfg, nil)
				for _, query := range []string{"first", "second", "third"} {
					svc.Record(t.Context(), Request{Query: query})
					time.Sleep(time.Second)
				}
				rows, err := svc.List(t.Context(), 3, 0)
				require.NoError(t, err)
				require.Len(t, rows, 3)
				missingID := rows[1].ID
				require.NoError(t, os.Remove(filepath.Join(root, "miss-log", svc.files[1])))
				if operation == "delete" {
					require.NoError(t, svc.Delete(t.Context(), missingID))
					require.ErrorIs(t, svc.Delete(t.Context(), missingID), os.ErrNotExist)
				}
				rows, err = svc.List(t.Context(), 2, 0)
				require.NoError(t, err)
				require.Len(t, rows, 2)
				require.Equal(t, "third", rows[0].Query)
				require.Equal(t, "first", rows[1].Query)
				require.ErrorIs(t, svc.Delete(t.Context(), missingID), os.ErrNotExist)
			})
		})
	}
}

func TestMissLogSkipsUnsupportedReceiptVersions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		cfg := config.Default().MissLog
		cfg.Enabled = true
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))
		svc := New(t.Context(), root, "synthetic-vault", cfg, logger)
		for _, query := range []string{"first", "second"} {
			svc.Record(t.Context(), Request{Query: query})
			time.Sleep(time.Second)
		}
		var paths []string
		for id, data := range map[string]string{
			"future":      `{"version":2,"query":"synthetic secret"}`,
			"unversioned": `{"query":"synthetic secret"}`,
		} {
			path := filepath.Join(root, "miss-log", time.Now().UTC().Format(fileTimeLayout)+"-"+id+".json")
			require.NoError(t, os.WriteFile(path, []byte(data), 0600))
			paths = append(paths, path)
		}
		svc = New(t.Context(), root, "synthetic-vault", cfg, logger)
		rows, err := svc.List(t.Context(), 2, 0)
		require.NoError(t, err)
		require.Len(t, rows, 2)
		require.Equal(t, "second", rows[0].Query)
		require.Equal(t, "first", rows[1].Query)
		require.Equal(t, 2, bytes.Count(logs.Bytes(), []byte("level=WARN")))
		require.NotContains(t, logs.String(), "synthetic secret")
		for _, path := range paths {
			require.FileExists(t, path)
		}
		rows, err = svc.List(t.Context(), 1, 1)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, "first", rows[0].Query)
		require.NoError(t, svc.Delete(t.Context(), "future"))
	})
}

func TestRecordingPreservesAssetScope(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "flat", true: "nested"}[nested], func(t *testing.T) {
			root := t.TempDir()
			cfg := config.Default().MissLog
			cfg.Enabled = true
			svc := New(t.Context(), root, "synthetic-vault", cfg, nil)
			for _, ids := range [][]any{{"synthetic-b", "synthetic-a"}, {"synthetic-c", "synthetic-d"}, {"synthetic-a", "synthetic-b"}} {
				filters := map[string]any{"asset_ids": ids, "tag_id": "synthetic-tag"}
				wantFilters := map[string]any{"asset_ids": slices.Clone(ids), "tag_id": "synthetic-tag"}
				if nested {
					filters = map[string]any{"filters": filters}
					wantFilters = map[string]any{"filters": wantFilters}
				}
				svc.Record(t.Context(), Request{Filters: filters})
				require.Equal(t, wantFilters, filters)
			}
			rows, err := New(t.Context(), root, "synthetic-vault", cfg, nil).List(t.Context(), 10, 0)
			require.NoError(t, err)
			require.Len(t, rows, 3)
			scopes := make([]map[string]any, len(rows))
			for i, row := range rows {
				scopes[i] = row.Filters
				if nested {
					var ok bool
					scopes[i], ok = row.Filters["filters"].(map[string]any)
					require.True(t, ok)
				}
				require.NotContains(t, scopes[i], "asset_ids")
				require.InDelta(t, 2, scopes[i]["asset_count"], 0)
			}
			require.Equal(t, "sha256:f97ef24088afcccfd22da4774be2c496a25e2f81f41b762c64e1c7986e79101b", scopes[0]["asset_fingerprint"])
			require.Equal(t, scopes[0], scopes[2], "reordering the same selection preserves its identity")
			require.NotEqual(t, scopes[0], scopes[1], "different selections retain different identities")
		})
	}
}

func TestRecordingSkipsEmptyStoredScope(t *testing.T) {
	for name, filters := range map[string]map[string]any{
		"empty list":     {"asset_ids": []any{}},
		"null list":      {"asset_ids": nil},
		"invalid list":   {"asset_ids": "synthetic-asset"},
		"nested empty":   {"filters": map[string]any{"asset_ids": []any{}}},
		"nested invalid": {"filters": map[string]any{"asset_ids": []any{42}}},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			cfg := config.Default().MissLog
			cfg.Enabled = true
			svc := New(t.Context(), root, "synthetic-vault", cfg, nil)
			svc.Record(t.Context(), Request{Query: " \n\t", Filters: filters})
			rows, err := New(t.Context(), root, "synthetic-vault", cfg, nil).List(t.Context(), 10, 0)
			require.NoError(t, err)
			require.Empty(t, rows)
		})
	}
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
		stagingPath := filepath.Join(root, "miss-log", ".receipt.json.tmp-interrupted")
		require.NoError(t, os.WriteFile(stagingPath, []byte("synthetic query bytes"), 0600))
		disabled := New(t.Context(), root, "synthetic-vault", cfg, nil)
		require.NoFileExists(t, stagingPath)
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
