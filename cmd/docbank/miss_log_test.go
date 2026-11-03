package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestMissLogRejectsUnboundedOrAmbiguousManagement(t *testing.T) {
	for _, args := range [][]string{{"list", "--limit", "0"}, {"list", "--limit", "101"}, {"list", "--offset", "-1"}, {"delete"}, {"delete", "record", "--before", "2026-01-01T00:00:00Z"}, {"delete", "--before", "yesterday"}, {"show"}} {
		cmd := newMissLogCmd()
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		require.Error(t, err, args)
		if args[0] != "show" {
			var exit *exitError
			require.ErrorAs(t, err, &exit, args)
			require.Equal(t, exitUsage, exit.code, args)
		}
	}
}

func TestMissLogRunSwitchEnablesOnlyCurrentDaemon(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)

	previous := daemonMissLog
	daemonMissLog = true
	t.Cleanup(func() { daemonMissLog = previous })
	stop := startServe(t)
	record := waitForDaemon(t, root)
	c := daemonconn.New("http://"+record.Address, record.Metadata["api_key"])
	result, err := c.Search(t.Context(), "synthetic run switch miss", 20)
	require.NoError(t, err)
	require.NotEmpty(t, result.MissLogID)
	records, err := c.ListMissLog(t.Context(), 20, 0)
	require.NoError(t, err)
	require.Len(t, records.Items, 1)
	stop()
	cfg, err := config.Load(root)
	require.NoError(t, err)
	require.False(t, cfg.MissLog.Enabled)
}

func TestMissLogLabelUsesMasterDaemon(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	previous := daemonMissLog
	daemonMissLog = true
	t.Cleanup(func() { daemonMissLog = previous })
	stop := startServe(t)
	defer stop()
	record := waitForDaemon(t, root)
	c := daemonconn.New("http://"+record.Address, record.Metadata["api_key"])
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	result, err := c.Search(t.Context(), "synthetic label miss", 20)
	require.NoError(t, err)
	cmd := newMissLogCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"label", result.MissLogID, "--verdict", "low_confidence", "--note", "synthetic correction"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	require.NotContains(t, output.String(), "synthetic")
	got, err := c.MissLogRecord(t.Context(), result.MissLogID)
	require.NoError(t, err)
	require.Equal(t, "low_confidence", got.Feedback.Verdict)
	for _, args := range [][]string{{"label", result.MissLogID, "--verdict", "none_in_vault", "--expected", "node:3"}, {"label", result.MissLogID, "--verdict", "wrong", "--expected", "node:0"}, {"label", result.MissLogID, "--verdict", "wrong", "--note", strings.Repeat("x", 1001)}} {
		cmd := newMissLogCmd()
		cmd.SetArgs(args)
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		require.Error(t, cmd.ExecuteContext(t.Context()))
	}
}
