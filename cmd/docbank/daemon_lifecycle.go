package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/home"
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the docbank daemon",
}

// printEnsured reports what client.EnsureDaemon found or did.
func printEnsured(cmd *cobra.Command, res client.EnsureResult) {
	if res.Replaced != nil {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "replaced daemon %s (pid %d) with %s: pid %d at %s\n",
			res.Replaced.Version, res.Replaced.PID, res.Record.Version, res.Record.PID, res.Record.Address)
		return
	}
	if res.Started {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "started: pid %d at %s (%s)\n",
			res.Record.PID, res.Record.Address, res.Record.Version)
		return
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "already running: pid %d at %s (%s)\n",
		res.Record.PID, res.Record.Address, res.Record.Version)
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon in the background",
	Long: "Start a daemon for this vault in the background, replacing a running daemon " +
		"whose version or protocol does not match this binary. Same convergence as the data commands' " +
		"auto-start: after `daemon start` succeeds, the one running daemon is current.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		if err := layout.Ensure(); err != nil {
			return err
		}
		res, err := client.EnsureDaemon(cmd.Context(), layout.Root)
		if err != nil {
			return err
		}
		printEnsured(cmd, res)
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
		res, err := client.EnsureDaemon(cmd.Context(), layout.Root)
		if err != nil {
			return err
		}
		rec := res.Record
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
