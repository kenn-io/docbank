package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMissLogRejectsUnboundedOrAmbiguousManagement(t *testing.T) {
	for _, args := range [][]string{{"list", "--limit", "0"}, {"list", "--limit", "101"}, {"list", "--offset", "-1"}, {"delete"}, {"show"}} {
		cmd := newMissLogCmd()
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		require.Error(t, err, args)
		if args[0] == "list" {
			var exit *exitError
			require.ErrorAs(t, err, &exit, args)
			require.Equal(t, exitUsage, exit.code, args)
		}
	}
}
