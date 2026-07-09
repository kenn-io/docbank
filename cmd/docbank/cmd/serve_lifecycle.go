package cmd

import (
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

// printAlreadyRunning reports an existing daemon, with a replacement hint
// when its version differs from this CLI: serve start never stops a running
// daemon (only the data commands' auto-start path replaces on mismatch).
func printAlreadyRunning(cmd *cobra.Command, rec kitdaemon.RuntimeRecord) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "already running: pid %d at %s (%s)\n",
		rec.PID, rec.Address, rec.Version)
	if rec.Version != version.Version {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"note: daemon version differs from this CLI (%s); `docbank serve stop && docbank serve start` to replace it\n",
			version.Version)
	}
}

var serveStartCmd = &cobra.Command{
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
		rec, _, ok, err := client.Find(cmd.Context(), layout.Root)
		if err != nil {
			return err
		}
		if ok {
			printAlreadyRunning(cmd, rec)
			return nil
		}
		err = client.WithLaunchLock(cmd.Context(), layout.Root, func() error {
			rec, _, ok, err = client.Find(cmd.Context(), layout.Root)
			if err != nil || ok {
				return err
			}
			rec, err = client.Start(cmd.Context(), layout.Root)
			return err
		})
		if err != nil {
			return err
		}
		if ok {
			printAlreadyRunning(cmd, rec)
			return nil
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "started: pid %d at %s (%s)\n",
			rec.PID, rec.Address, rec.Version)
		return nil
	},
}

var serveStatusJSON bool

var serveStatusCmd = &cobra.Command{
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
		if serveStatusJSON {
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

var serveStopCmd = &cobra.Command{
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

func init() {
	serveStatusCmd.Flags().BoolVar(&serveStatusJSON, "json", false, "machine-readable output")
	serveCmd.AddCommand(serveStartCmd, serveStatusCmd, serveStopCmd)
}
