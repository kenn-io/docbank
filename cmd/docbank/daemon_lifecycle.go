package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/version"
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the docbank daemon",
}

// printAlreadyRunning reports an existing daemon, with a replacement hint
// when its version differs from this CLI: daemon start never stops a
// running daemon (only the data commands' auto-start path replaces on
// mismatch).
func printAlreadyRunning(cmd *cobra.Command, rec kitdaemon.RuntimeRecord) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "already running: pid %d at %s (%s)\n",
		rec.PID, rec.Address, rec.Version)
	if rec.Version != version.Version {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"note: daemon version differs from this CLI (%s); `docbank daemon stop && docbank daemon start` to replace it\n",
			version.Version)
	}
}

// startOrFind returns the already-running daemon if discovery finds one, or
// starts a fresh one under the launch lock (re-checking discovery inside the
// lock so a racing starter is never duplicated). `daemon start` and `daemon
// restart` share this path so both get the same launch-lock and
// version-match semantics.
func startOrFind(ctx context.Context, root string) (rec kitdaemon.RuntimeRecord, alreadyRunning bool, err error) {
	rec, _, ok, err := client.Find(ctx, root)
	if err != nil {
		return kitdaemon.RuntimeRecord{}, false, err
	}
	if ok {
		return rec, true, nil
	}
	err = client.WithLaunchLock(ctx, root, func() error {
		rec, _, ok, err = client.Find(ctx, root)
		if err != nil || ok {
			return err
		}
		rec, err = client.Start(ctx, root)
		return err
	})
	if err != nil {
		return kitdaemon.RuntimeRecord{}, false, err
	}
	return rec, ok, nil
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon in the background",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		if err := layout.Ensure(); err != nil {
			return err
		}
		rec, alreadyRunning, err := startOrFind(cmd.Context(), layout.Root)
		if err != nil {
			return err
		}
		if alreadyRunning {
			printAlreadyRunning(cmd, rec)
			return nil
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "started: pid %d at %s (%s)\n",
			rec.PID, rec.Address, rec.Version)
		return nil
	},
}

var daemonStatusJSON bool

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report the daemon's status (never starts one)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		rec, info, ok, err := client.Find(cmd.Context(), layout.Root)
		if err != nil {
			return err
		}
		if daemonStatusJSON {
			out := map[string]any{"running": ok}
			if ok {
				out["pid"] = rec.PID
				out["address"] = rec.Address
				out["version"] = info.Version
				out["started_at"] = rec.StartedAt.Format(time.RFC3339)
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}
		if !ok {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "daemon not running")
			return errors.New("daemon not running")
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "running: pid %d at %s (%s), up %s\n",
			rec.PID, rec.Address, info.Version, time.Since(rec.StartedAt).Round(time.Second))
		return nil
	},
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running daemon (never starts one)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		stopped, err := client.Stop(cmd.Context(), layout.Root)
		if err != nil {
			return err
		}
		if !stopped {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no daemon running")
			return nil
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "stopped")
		return nil
	},
}

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the daemon",
	Long:  "Stop the daemon if one is running, then start it again. Tolerates the daemon not already running.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		wasRunning, err := client.Stop(cmd.Context(), layout.Root)
		if err != nil {
			return err
		}
		if err := layout.Ensure(); err != nil {
			return err
		}
		rec, _, err := startOrFind(cmd.Context(), layout.Root)
		if err != nil {
			return err
		}
		if wasRunning {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "restarted: pid %d at %s (%s)\n",
				rec.PID, rec.Address, rec.Version)
		} else {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "started (was not running): pid %d at %s (%s)\n",
				rec.PID, rec.Address, rec.Version)
		}
		return nil
	},
}

func init() {
	daemonStatusCmd.Flags().BoolVar(&daemonStatusJSON, "json", false, "machine-readable output")
	daemonCmd.AddCommand(daemonRunCmd, daemonStartCmd, daemonStatusCmd, daemonStopCmd, daemonRestartCmd)
	rootCmd.AddCommand(daemonCmd)
}
