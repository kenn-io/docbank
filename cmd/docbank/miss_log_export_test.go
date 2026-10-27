package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/misslog"
)

func TestMissLogExportFailureLeavesNoOutput(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "questions.csv")
	err := writePrivateMissLogExport(target, func(w io.Writer) error {
		_, err := io.WriteString(w, "synthetic private partial data")
		require.NoError(t, err)
		return errors.New("synthetic failure")
	})
	require.ErrorContains(t, err, "synthetic failure")
	_, err = os.Stat(target)
	require.ErrorIs(t, err, os.ErrNotExist)
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestMissLogExportWritesExclusivePrivateFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	previous := daemonMissLog
	daemonMissLog = true
	t.Cleanup(func() { daemonMissLog = previous })
	stop := startServe(t)
	defer stop()
	record := waitForDaemon(t, root)
	connection := daemonconn.New("http://"+record.Address, record.Metadata["api_key"])
	defer connection.Close()
	query := "=synthetic,private\nquestion"
	report, err := connection.Search(t.Context(), query, 20)
	require.NoError(t, err)
	require.NotEmpty(t, report.MissLogID)
	require.NoError(t, connection.LabelMissLogRecord(t.Context(), report.MissLogID, misslog.Feedback{Verdict: "none_in_vault", Note: "private note"}))
	_, err = connection.Search(t.Context(), "synthetic unlabeled question", 20)
	require.NoError(t, err)
	for _, format := range []string{"eval", "cards"} {
		t.Run(format, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "questions.out")
			var output bytes.Buffer
			command := newMissLogCmd()
			command.SetOut(&output)
			command.SetArgs([]string{"export", format, target})
			require.NoError(t, command.ExecuteContext(t.Context()))
			require.NotContains(t, output.String(), "synthetic")
			require.NotContains(t, output.String(), "private note")
			require.Contains(t, output.String(), "skipped unlabeled: 1")
			data, err := os.ReadFile(target)
			require.NoError(t, err)
			info, err := os.Stat(target)
			require.NoError(t, err)
			if runtime.GOOS != "windows" {
				require.Zero(t, info.Mode().Perm()&0o077)
			}
			if format == "eval" {
				rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
				require.NoError(t, err)
				require.Equal(t, []string{"id", "question", "expected_ids", "scope"}, rows[0])
				require.Equal(t, query, rows[1][1])
				require.Empty(t, rows[1][2])
			} else {
				var card map[string]any
				require.NoError(t, json.Unmarshal(data, &card))
				require.Equal(t, query, card["question"])
				require.Equal(t, []any{}, card["expected"])
			}
			command = newMissLogCmd()
			command.SilenceErrors = true
			command.SilenceUsage = true
			command.SetArgs([]string{"export", format, target})
			require.Error(t, command.ExecuteContext(t.Context()))
			unchanged, err := os.ReadFile(target)
			require.NoError(t, err)
			require.Equal(t, data, unchanged)
		})
	}
	for _, args := range [][]string{{"export", "eval"}, {"export", "unknown", "out"}, {"export", "eval", "-"}} {
		command := newMissLogCmd()
		command.SilenceErrors = true
		command.SilenceUsage = true
		command.SetArgs(args)
		require.Error(t, command.ExecuteContext(t.Context()))
	}
}
