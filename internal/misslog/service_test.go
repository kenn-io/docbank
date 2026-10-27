package misslog

import (
	"bytes"
	"context"
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
	disabled := New(st, cfg, logger)
	require.Empty(t, disabled.Record(t.Context(), Receipt{Query: "synthetic secret"}))
	cfg.Enabled = true
	svc := New(st, cfg, logger)
	id := svc.Record(t.Context(), Receipt{Query: "synthetic secret", Scope: map[string]any{"empty": true}})
	require.NotEmpty(t, id)
	require.Empty(t, svc.Record(t.Context(), Receipt{Query: strings.Repeat("x", 32769)}))
	require.Contains(t, logs.String(), "miss_log_write_failed")
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
	New(st, cfg, nil)
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	cfg.Enabled = true
	New(st, cfg, nil)
	rows, err = st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestRecordingHonorsMutationAdmission(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	cfg := config.Default().MissLog
	cfg.Enabled = true
	calls := 0
	svc := New(st, cfg, nil, func(ctx context.Context, fn func() error) error { calls++; return context.Canceled })
	require.Empty(t, svc.Record(t.Context(), Receipt{Query: "private blocked search"}))
	require.Positive(t, calls)
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestRecordingQueryLimitCountsUnicodeCharacters(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	cfg := config.Default().MissLog
	cfg.Enabled = true
	svc := New(st, cfg, nil)
	query := strings.Repeat("界", 32768)
	id := svc.Record(t.Context(), Receipt{Query: query})
	require.NotEmpty(t, id, "valid character-bounded query must be retained")
	row, err := st.MissLogRecordByID(t.Context(), id)
	require.NoError(t, err)
	require.Contains(t, row.Receipt, query)
	require.Empty(t, svc.Record(t.Context(), Receipt{Query: query + "界"}))
}
