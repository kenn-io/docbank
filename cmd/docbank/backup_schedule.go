package main

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

var backupScheduleRevision int64

var backupScheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Inspect or override the backup schedule without a restart",
	Long: `Overrides take precedence over [backup] interval and keep_last in config.toml
and apply without a daemon restart. Keys: interval, a duration such as 24h where
0 disables scheduling, and keep_last. Reset a key to inherit config.toml again.
The repository path stays in config.toml. Output: JSON.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var backupScheduleShowCmd = &cobra.Command{
	Use: "show", Short: "Show overrides and the schedule in force", Args: cobra.NoArgs,
	Example: `  docbank backup schedule show`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		schedule, err := c.BackupSchedule(cmd.Context())
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), schedule)
	},
}

var backupScheduleSetCmd = &cobra.Command{
	Use: "set <interval|keep_last> <value>", Short: "Override the interval or retention", Args: cobra.ExactArgs(2),
	Example: `  docbank backup schedule set interval 1h
  docbank backup schedule set keep_last 5`,
	RunE: func(cmd *cobra.Command, args []string) error {
		var request api.SetBackupScheduleRequest
		switch args[0] {
		case "interval":
			seconds, err := wholeSeconds(args[1])
			if err != nil {
				return err
			}
			request.IntervalSeconds = &seconds
		case "keep_last":
			keep, err := strconv.Atoi(args[1])
			if err != nil || keep < 1 {
				return usageError(errors.New("keep_last requires a positive integer"))
			}
			request.KeepLast = &keep
		default:
			return unknownBackupScheduleKey(args[0])
		}
		return writeBackupSchedule(cmd, request)
	},
}

var backupScheduleResetCmd = &cobra.Command{
	Use: "reset <interval|keep_last>", Short: "Inherit a key from config.toml again", Args: cobra.ExactArgs(1),
	Example: `  docbank backup schedule reset interval`,
	RunE: func(cmd *cobra.Command, args []string) error {
		var request api.SetBackupScheduleRequest
		switch args[0] {
		case "interval":
			request.ResetInterval = true
		case "keep_last":
			request.ResetKeepLast = true
		default:
			return unknownBackupScheduleKey(args[0])
		}
		return writeBackupSchedule(cmd, request)
	},
}

func unknownBackupScheduleKey(key string) error {
	return usageError(fmt.Errorf("unknown schedule key %q; use interval or keep_last, and set the repository in config.toml", key))
}

// writeBackupSchedule sends an explicit --revision unchanged. Without one it
// reads the current revision and retries once if a concurrent write made it stale.
func writeBackupSchedule(cmd *cobra.Command, request api.SetBackupScheduleRequest) error {
	explicit := cmd.Flags().Changed("revision")
	if explicit && backupScheduleRevision < 1 {
		return usageError(errors.New("--revision must be a positive integer"))
	}
	c, err := daemonconn.Ensure(cmd.Context())
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		revision := backupScheduleRevision
		if !explicit {
			current, err := c.BackupSchedule(cmd.Context())
			if err != nil {
				return err
			}
			revision = current.Revision
		}
		schedule, err := c.SetBackupSchedule(cmd.Context(), revision, request)
		if !explicit && attempt == 0 && errors.Is(err, store.ErrStaleRevision) {
			continue
		}
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), schedule)
	}
}

func init() {
	for _, command := range []*cobra.Command{backupScheduleSetCmd, backupScheduleResetCmd} {
		command.Flags().Int64Var(&backupScheduleRevision, "revision", 0, "expected settings revision (default: current, retried once)")
	}
	backupScheduleCmd.AddCommand(backupScheduleShowCmd, backupScheduleSetCmd, backupScheduleResetCmd)
	backupCmd.AddCommand(backupScheduleCmd)
}
