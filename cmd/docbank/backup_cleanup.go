package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

var (
	backupForgetRepo        string
	backupForgetDryRun      bool
	backupForgetAllowEmpty  bool
	backupForgetForceUnlock bool
	backupForgetJSON        bool
	backupPruneRepo         string
	backupPruneDryRun       bool
	backupPruneForceUnlock  bool
	backupPruneJSON         bool
)

var backupForgetCmd = &cobra.Command{
	Use:     "forget <snapshot> [<snapshot>...]",
	Short:   "Remove selected backup recovery points",
	Example: "  docbank backup forget 20260101T000000Z-11111111111111111111111111111111 --repo ~/backup --dry-run",
	Args:    cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, err := absoluteBackupRepo(backupForgetRepo)
		if err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		report, err := c.API().ForgetBackupSnapshots(cmd.Context(), &apiclient.ForgetBackupSnapshotsRequestOptions{Body: &apiclient.ForgetBackupSnapshotsBody{
			Repo: new(repo), SnapshotIds: args, DryRun: new(backupForgetDryRun), AllowEmpty: new(backupForgetAllowEmpty), ForceUnlock: new(backupForgetForceUnlock),
		}})
		if err != nil {
			if partial, ok := errors.AsType[*daemonconn.BackupCleanupError](err); ok && partial.Forget != nil && partial.Forget.HasWork() {
				if outputErr := writeBackupForgetReport(cmd.OutOrStdout(), *partial.Forget, backupForgetJSON); outputErr != nil {
					return errors.Join(err, outputErr)
				}
			}
			return err
		}
		return writeBackupForgetReport(cmd.OutOrStdout(), *report, backupForgetJSON)
	},
}

var backupPruneCmd = &cobra.Command{
	Use:     "prune",
	Short:   "Reclaim unused backup storage",
	Example: "  docbank backup prune --repo ~/backup --dry-run\n  docbank backup prune --repo ~/backup --json",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		repo, err := absoluteBackupRepo(backupPruneRepo)
		if err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		report, err := c.API().PruneBackupRepository(cmd.Context(), &apiclient.PruneBackupRepositoryRequestOptions{Body: &apiclient.PruneBackupRepositoryBody{
			Repo: new(repo), DryRun: new(backupPruneDryRun), ForceUnlock: new(backupPruneForceUnlock),
		}})
		if err != nil {
			if partial, ok := errors.AsType[*daemonconn.BackupCleanupError](err); ok && partial.Prune != nil && partial.Prune.HasWork() {
				if outputErr := writeBackupPruneReport(cmd.OutOrStdout(), *partial.Prune, backupPruneJSON); outputErr != nil {
					return errors.Join(err, outputErr)
				}
			}
			return err
		}
		return writeBackupPruneReport(cmd.OutOrStdout(), *report, backupPruneJSON)
	},
}

func init() {
	backupForgetCmd.Flags().StringVar(&backupForgetRepo, "repo", "", "backup repository directory")
	backupForgetCmd.Flags().BoolVar(&backupForgetDryRun, "dry-run", false, "report the selection without removing snapshots")
	backupForgetCmd.Flags().BoolVar(&backupForgetAllowEmpty, "allow-empty", false, "allow removal of the last recovery point")
	backupForgetCmd.Flags().BoolVar(&backupForgetForceUnlock, "force-unlock", false, "break a repository lock whose owner is known to be gone")
	backupForgetCmd.Flags().BoolVar(&backupForgetJSON, "json", false, "print JSON to stdout")
	backupPruneCmd.Flags().StringVar(&backupPruneRepo, "repo", "", "backup repository directory")
	backupPruneCmd.Flags().BoolVar(&backupPruneDryRun, "dry-run", false, "report planned cleanup without changing storage")
	backupPruneCmd.Flags().BoolVar(&backupPruneForceUnlock, "force-unlock", false, "break a repository lock whose owner is known to be gone")
	backupPruneCmd.Flags().BoolVar(&backupPruneJSON, "json", false, "print JSON to stdout")
}

func writeBackupForgetReport(w io.Writer, report api.BackupForgetReport, asJSON bool) error {
	if asJSON {
		return writeBackupJSON(w, report)
	}
	_, err := fmt.Fprintf(w, "selected %d snapshot(s); forgotten %d\nselected snapshots: %s\nforgotten snapshots: %s\n", len(report.Selected), len(report.Forgotten), strings.Join(report.Selected, ", "), strings.Join(report.Forgotten, ", "))
	if err != nil {
		return fmt.Errorf("writing backup cleanup output: %w", err)
	}
	return nil
}

func writeBackupPruneReport(w io.Writer, report api.BackupPruneReport, asJSON bool) error {
	if asJSON {
		return writeBackupJSON(w, report)
	}
	_, err := fmt.Fprintf(w, "planned %d pack removal(s), %d rewrite(s); removed %d pack(s), %d byte(s); wrote %d byte(s)\nplanned removal bytes: %d; live bytes to rewrite: %d\nplanned pack removals: %s\nplanned pack rewrites: %s\nremoved packs: %s\n", len(report.PacksToRemove), len(report.PacksToRepack), len(report.RemovedPacks), report.BytesRemoved, report.BytesWritten, report.BytesToRemove, report.LiveBytesToRewrite, strings.Join(report.PacksToRemove, ", "), strings.Join(report.PacksToRepack, ", "), strings.Join(report.RemovedPacks, ", "))
	if err != nil {
		return fmt.Errorf("writing backup cleanup output: %w", err)
	}
	return nil
}
