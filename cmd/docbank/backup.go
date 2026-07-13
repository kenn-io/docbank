package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Create and inspect immutable vault snapshots",
}

var (
	backupInitRepo string
	backupInitJSON bool
)

var backupInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a backup repository",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		repo, err := absoluteBackupRepo(backupInitRepo)
		if err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		created, err := c.BackupInit(cmd.Context(), repo)
		if err != nil {
			return err
		}
		if backupInitJSON {
			return writeBackupJSON(cmd.OutOrStdout(), created)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "initialized backup repository %s at %s\n",
			created.ID, created.Path)
		if err != nil {
			return fmt.Errorf("writing backup init output: %w", err)
		}
		return nil
	},
}

var (
	backupCreateRepo        string
	backupCreateTag         string
	backupCreateJobs        int
	backupCreateForceUnlock bool
	backupCreateJSON        bool
	backupCreateProgress    string
)

var backupCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a verified snapshot of the live vault",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		repo, err := absoluteBackupRepo(backupCreateRepo)
		if err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		opts := client.BackupCreateOptions{
			Repo: repo, Tag: backupCreateTag, Jobs: backupCreateJobs,
			ForceUnlock: backupCreateForceUnlock,
		}
		var snapshot api.BackupSnapshot
		if backupCreateJSON {
			snapshot, err = c.BackupCreate(cmd.Context(), opts)
		} else {
			mode, modeErr := backupProgressModeFromFlag(backupCreateProgress)
			if modeErr != nil {
				return modeErr
			}
			renderer := newBackupProgressRenderer(cmd.ErrOrStderr(), mode)
			defer renderer.finish()
			snapshot, err = c.BackupCreateStream(cmd.Context(), opts, renderer.handle)
		}
		if err != nil {
			return err
		}
		if backupCreateJSON {
			return writeBackupJSON(cmd.OutOrStdout(), snapshot)
		}
		parent := snapshot.ParentID
		if parent == "" {
			parent = "initial"
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(),
			"created snapshot %s (parent: %s); %d file(s), %d blob(s), %d byte(s) added\n",
			snapshot.ID, parent, snapshot.Files, snapshot.Blobs, snapshot.BytesAdded)
		if err != nil {
			return fmt.Errorf("writing backup create output: %w", err)
		}
		return nil
	},
}

var (
	backupListRepo string
	backupListJSON bool
)

var backupListCmd = &cobra.Command{
	Use:   "list",
	Short: "List backup snapshots",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		repo, err := absoluteBackupRepo(backupListRepo)
		if err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		snapshots, err := c.BackupList(cmd.Context(), repo)
		if err != nil {
			return err
		}
		if backupListJSON {
			return writeBackupJSON(cmd.OutOrStdout(), api.BackupSnapshotList{Items: snapshots})
		}
		return writeBackupList(cmd.OutOrStdout(), snapshots)
	},
}

var (
	backupVerifyRepo        string
	backupVerifyAll         bool
	backupVerifyQuick       bool
	backupVerifyJobs        int
	backupVerifyForceUnlock bool
	backupVerifyJSON        bool
	backupVerifyProgress    string
)

var backupVerifyCmd = &cobra.Command{
	Use:   "verify [SNAPSHOT]",
	Short: "Verify backup repository integrity",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if backupVerifyAll && len(args) > 0 {
			return errors.New("backup verify: SNAPSHOT and --all are mutually exclusive")
		}
		repo, err := absoluteBackupRepo(backupVerifyRepo)
		if err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		opts := client.BackupVerifyOptions{
			Repo: repo, All: backupVerifyAll, Quick: backupVerifyQuick,
			Jobs: backupVerifyJobs, ForceUnlock: backupVerifyForceUnlock,
		}
		if len(args) == 1 {
			opts.SnapshotID = args[0]
		}
		var report api.BackupVerifyReport
		if backupVerifyJSON {
			report, err = c.BackupVerify(cmd.Context(), opts)
		} else {
			mode, modeErr := backupProgressModeFromFlag(backupVerifyProgress)
			if modeErr != nil {
				return modeErr
			}
			renderer := newBackupProgressRenderer(cmd.ErrOrStderr(), mode)
			defer renderer.finish()
			report, err = c.BackupVerifyStream(cmd.Context(), opts, renderer.handle)
		}
		if err != nil {
			return err
		}
		if backupVerifyJSON {
			if err := writeBackupJSON(cmd.OutOrStdout(), report); err != nil {
				return err
			}
		} else if err := writeBackupVerifyReport(cmd.OutOrStdout(), report); err != nil {
			return err
		}
		if len(report.Problems) > 0 {
			return fmt.Errorf("backup verify: found %d problem(s)", len(report.Problems))
		}
		return nil
	},
}

func absoluteBackupRepo(repo string) (string, error) {
	if repo == "" {
		return "", nil
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", fmt.Errorf("resolving backup repository %q: %w", repo, err)
	}
	return filepath.Clean(abs), nil
}

func writeBackupJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return fmt.Errorf("writing backup JSON: %w", err)
	}
	return nil
}

func writeBackupList(w io.Writer, snapshots []api.BackupSnapshot) error {
	if len(snapshots) == 0 {
		_, err := fmt.Fprintln(w, "no snapshots found")
		if err != nil {
			return fmt.Errorf("writing empty backup list: %w", err)
		}
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "SNAPSHOT\tCREATED\tFILES\tBLOBS\tBYTES ADDED\tTAG"); err != nil {
		return fmt.Errorf("writing backup list header: %w", err)
	}
	for _, snapshot := range snapshots {
		tag := snapshot.Tag
		if tag == "" {
			tag = "-"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\n",
			snapshot.ID, snapshot.CreatedAt, snapshot.Files, snapshot.Blobs,
			snapshot.BytesAdded, tag); err != nil {
			return fmt.Errorf("writing backup list row: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing backup list: %w", err)
	}
	return nil
}

func writeBackupVerifyReport(w io.Writer, report api.BackupVerifyReport) error {
	for _, problem := range report.Problems {
		if _, err := fmt.Fprintf(w, "problem: snapshot %s: %s\n",
			problem.SnapshotID, problem.Detail); err != nil {
			return fmt.Errorf("writing backup verification problem: %w", err)
		}
	}
	if _, err := fmt.Fprintf(w,
		"verified %d snapshot(s), %d blob(s), %s read; %d problem(s)\n",
		len(report.Snapshots), report.BlobsChecked, formatBackupBytes(report.BytesRead),
		len(report.Problems)); err != nil {
		return fmt.Errorf("writing backup verification summary: %w", err)
	}
	return nil
}

func init() {
	backupInitCmd.Flags().StringVar(&backupInitRepo, "repo", "", "backup repository directory")
	backupInitCmd.Flags().BoolVar(&backupInitJSON, "json", false, "machine-readable output")
	backupCreateCmd.Flags().StringVar(&backupCreateRepo, "repo", "", "backup repository directory")
	backupCreateCmd.Flags().StringVar(&backupCreateTag, "tag", "", "label recorded on the snapshot")
	backupCreateCmd.Flags().IntVar(&backupCreateJobs, "jobs", 0,
		"concurrent blob readers (0 uses one per CPU; use 1 for spinning disks or NAS shares)")
	backupCreateCmd.Flags().BoolVar(&backupCreateForceUnlock, "force-unlock", false,
		"break a fresh repository lock only when its owner is known to be gone")
	backupCreateCmd.Flags().BoolVar(&backupCreateJSON, "json", false, "machine-readable output")
	backupCreateCmd.Flags().StringVar(&backupCreateProgress, "progress", "auto",
		"progress output mode: auto, bar, or plain (suppressed by --json)")
	backupListCmd.Flags().StringVar(&backupListRepo, "repo", "", "backup repository directory")
	backupListCmd.Flags().BoolVar(&backupListJSON, "json", false, "machine-readable output")
	backupVerifyCmd.Flags().StringVar(&backupVerifyRepo, "repo", "", "backup repository directory")
	backupVerifyCmd.Flags().BoolVar(&backupVerifyAll, "all", false, "verify every snapshot")
	backupVerifyCmd.Flags().BoolVar(&backupVerifyQuick, "quick", false,
		"check repository structure without reading content bytes")
	backupVerifyCmd.Flags().IntVar(&backupVerifyJobs, "jobs", 0,
		"concurrent blob readers (0 uses one per CPU; use 1 for spinning disks or NAS shares)")
	backupVerifyCmd.Flags().BoolVar(&backupVerifyForceUnlock, "force-unlock", false,
		"break a fresh repository lock only when its owner is known to be gone")
	backupVerifyCmd.Flags().BoolVar(&backupVerifyJSON, "json", false, "machine-readable output")
	backupVerifyCmd.Flags().StringVar(&backupVerifyProgress, "progress", "auto",
		"progress output mode: auto, bar, or plain (suppressed by --json)")
	backupCmd.AddCommand(backupInitCmd, backupCreateCmd, backupListCmd, backupVerifyCmd)
	rootCmd.AddCommand(backupCmd)
}
