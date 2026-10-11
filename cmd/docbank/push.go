package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/push"
)

func newPushCmd() *cobra.Command {
	var target, name, dest, keyFile, duplicates string
	var watch bool
	var excludes []string
	var settle, age, interval time.Duration
	cmd := &cobra.Command{
		Use:   "push <local-dir> --to <daemon-url> --name <push-name> --dest <virtual-dir>",
		Short: "Push a local folder to a remote daemon",
		Long: `Archive a local folder into the vault of the daemon at --to, usually
through an SSH tunnel. The daemon needs no copy of the folder. --name and each
file's relative path identify the file: reruns upload only changed files, and
a change adds a version to the same document. Local deletions never delete
archived documents.

The API key comes from DOCBANK_API_KEY or --api-key-file. Each connection must
prove it holds that key before push sends the key or any file. Rejected files
are reported and skipped, and then the exit code is nonzero.`,
		Example: `  docbank push ~/documents --to http://127.0.0.1:7778 --name laptop --dest /documents
  docbank push ~/documents --to http://127.0.0.1:7778 --name laptop --dest /documents --watch`,
		GroupID: groupSources,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := readPushKey(keyFile)
			if err != nil {
				return err
			}
			connection, err := daemonconn.NewPushConnection(target, key)
			if err != nil {
				return fmt.Errorf("--to: %w", err)
			}
			return runPush(cmd, connection, push.Options{
				Folder: config.WatchConfig{
					Name: name, Source: args[0], Destination: dest, SettleTime: config.Duration(settle),
					MinimumAge: config.Duration(age), ScanInterval: config.Duration(interval), Exclude: excludes,
				},
				Duplicates: duplicates, Watch: watch,
			})
		},
	}
	cmd.Flags().StringVar(&target, "to", "", "daemon URL")
	cmd.Flags().StringVar(&name, "name", "", "stable push name (lowercase letters, digits, -, _, .)")
	cmd.Flags().StringVar(&dest, "dest", "", "absolute virtual destination")
	cmd.Flags().StringVar(&keyFile, "api-key-file", "", "read API key from a file instead of DOCBANK_API_KEY")
	cmd.Flags().StringVar(&duplicates, "duplicates", "link", "new-path duplicate policy: link, skip, create")
	cmd.Flags().BoolVar(&watch, "watch", false, "keep scanning with watched-inbox stability rules")
	cmd.Flags().StringArrayVar(&excludes, "exclude", nil, "literal basename or source-relative path to exclude (repeatable)")
	cmd.Flags().DurationVar(&settle, "settle-time", 30*time.Second, "unchanged window before uploading in watch mode")
	cmd.Flags().DurationVar(&age, "minimum-age", 0, "minimum time since source modification")
	cmd.Flags().DurationVar(&interval, "scan-interval", 5*time.Second, "watch polling interval")
	for _, flag := range []string{"to", "name", "dest"} {
		if err := cmd.MarkFlagRequired(flag); err != nil {
			panic(err)
		}
	}
	return cmd
}

func readPushKey(keyFile string) (string, error) {
	key := os.Getenv("DOCBANK_API_KEY")
	if keyFile != "" {
		value, err := os.ReadFile(keyFile) // #nosec G304 -- Explicit operator-selected credential file.
		if err != nil {
			return "", fmt.Errorf("reading API key file: %w", err)
		}
		key = strings.TrimSpace(string(value))
	}
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return "", errors.New("set DOCBANK_API_KEY or --api-key-file to a nonempty API key")
	}
	return key, nil
}

// runPush prints each outcome and a final summary. An interrupt stops the run
// cleanly: the summary still prints, and stopping a watch is not an error.
func runPush(cmd *cobra.Command, connection *daemonconn.Connection, opts push.Options) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts.Progress = func(ref, outcome string) error {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", outcome, strconv.QuoteToASCII(ref)); err != nil {
			return fmt.Errorf("writing push progress: %w", err)
		}
		return nil
	}
	opts.Failure = func(ref string, err error) error {
		if _, writeErr := fmt.Fprintf(cmd.ErrOrStderr(), "failed %s: %v\n", strconv.QuoteToASCII(ref), err); writeErr != nil {
			return fmt.Errorf("writing push failure: %w", writeErr)
		}
		return nil
	}
	report, runErr := push.Run(ctx, connection, opts)
	interrupted := ctx.Err() != nil && cmd.Context().Err() == nil
	if interrupted && opts.Watch && errors.Is(runErr, context.Canceled) {
		runErr = nil
		if report.Failed > 0 {
			runErr = fmt.Errorf("%w: %d failed", push.ErrFilesFailed, report.Failed)
		}
	}
	_, outputErr := fmt.Fprintf(cmd.OutOrStdout(),
		"added %d, updated %d, linked %d, unchanged %d, duplicate-skipped %d, failed %d\n",
		report.Added, report.Updated, report.Linked, report.Skipped, report.DuplicateSkipped, report.Failed)
	if outputErr != nil {
		outputErr = fmt.Errorf("writing push summary: %w", outputErr)
	}
	return errors.Join(runErr, outputErr)
}

func init() { rootCmd.AddCommand(newPushCmd()) }
