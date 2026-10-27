package main

import (
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
