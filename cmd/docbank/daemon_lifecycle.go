package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
)

var daemonCmd = &cobra.Command{
	Long: `Data commands start the daemon automatically and replace one from a different
binary version. Use daemon restart to keep an automatic daemon running until daemon stop.`,
	GroupID: groupOperations,
	Use:     "daemon",
	Short:   "Manage the docbank daemon",
}

// printEnsured reports what daemonconn.EnsureDaemon found or did.
func printEnsured(cmd *cobra.Command, res daemonconn.EnsureResult) {
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
	if !daemonconn.Explicit(res.Record) && (res.Record.Metadata[daemonconn.MetaIdleTimeout] != "" || res.Record.Metadata[daemonconn.MetaExplicitStart] == "") {
		if lifetime := daemonLifetime(res.Record); lifetime != "" {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), lifetime)
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Run docbank daemon restart to keep it running.")
	}
}

var daemonStartCmd = &cobra.Command{
	Example: `  docbank daemon start`,
	Use:     "start",
	Short:   "Start the daemon in the background",
	Long: `Start a daemon for this vault in the background, replacing a running daemon
whose version or protocol does not match this binary. Reuse a compatible
daemon with its existing idle policy. A new daemon stays running until
docbank daemon stop; use daemon restart to keep an automatic daemon running.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		res, err := daemonconn.EnsureDaemon(cmd.Context(), layout.Root, true)
		if err != nil {
			return err
		}
		printEnsured(cmd, res)
		return nil
	},
}

var daemonStatusJSON bool

var daemonStatusCmd = &cobra.Command{
	Example: `  docbank daemon status --json`,
	Use:     "status",
	Short:   "Report the daemon's status (never starts one)",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		rec, info, ok, err := daemonconn.Find(cmd.Context(), layout.Root)
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
				if timeout := rec.Metadata[daemonconn.MetaIdleTimeout]; timeout != "" {
					out["idle_timeout"] = timeout
					out["explicit_start"] = daemonconn.Explicit(rec)
				}
			}
			enc := jsontext.NewEncoder(cmd.OutOrStdout(), jsontext.WithIndent("  "))
			return json.MarshalEncode(enc, out)
		}
		if !ok {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "daemon not running")
			return errors.New("daemon not running")
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "running: pid %d at %s (%s), up %s\n",
			rec.PID, rec.Address, info.Version, time.Since(rec.StartedAt).Round(time.Second))
		if lifetime := daemonLifetime(rec); lifetime != "" {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), lifetime)
		}
		return nil
	},
}

func daemonLifetime(rec kitdaemon.RuntimeRecord) string {
	timeout, err := time.ParseDuration(rec.Metadata[daemonconn.MetaIdleTimeout])
	if err != nil {
		return unavailableDaemonLifetime(rec)
	}
	if daemonconn.Explicit(rec) {
		return "Stays running until docbank daemon stop"
	}
	if timeout == 0 {
		return "Idle exit is off (started automatically)"
	}
	duration := timeout.String()
	if timeout%time.Minute == 0 {
		duration = fmt.Sprintf("%dm", timeout/time.Minute)
	}
	return fmt.Sprintf("Exits after %s idle (started automatically)", duration)
}

func unavailableDaemonLifetime(rec kitdaemon.RuntimeRecord) string {
	if rec.Metadata[daemonconn.MetaExplicitStart] == "" {
		return "Lifetime unavailable (daemon predates lifetime reporting)"
	}
	return ""
}

var daemonStopCmd = &cobra.Command{
	Example: `  docbank daemon stop`,
	Use:     "stop",
	Short:   "Stop the running daemon (never starts one)",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		stopped, err := daemonconn.Stop(cmd.Context(), layout.Root)
		if err != nil {
			return err
		}
		if stopped == nil {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no daemon running")
			return nil
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "stopped")
		return nil
	},
}

var daemonRestartCmd = &cobra.Command{
	Example: `  docbank daemon restart`,
	Use:     "restart",
	Short:   "Restart the daemon",
	Long: `Stop the daemon if one is running, then start it again. Succeeds when no
daemon was running.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		rec, wasRunning, err := daemonconn.Restart(cmd.Context(), layout.Root, true)
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
	daemonStatusCmd.Flags().BoolVar(&daemonStatusJSON, "json", false, "print JSON to stdout")
	daemonCmd.AddCommand(daemonRunCmd, daemonStartCmd, daemonStatusCmd, daemonStopCmd, daemonRestartCmd)
	rootCmd.AddCommand(daemonCmd)
}
