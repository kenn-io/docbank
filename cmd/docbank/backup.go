package main

import (
	"encoding/json"
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
		snapshot, err := c.BackupCreate(cmd.Context(), client.BackupCreateOptions{
			Repo: repo, Tag: backupCreateTag, Jobs: backupCreateJobs,
			ForceUnlock: backupCreateForceUnlock,
		})
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
	backupListCmd.Flags().StringVar(&backupListRepo, "repo", "", "backup repository directory")
	backupListCmd.Flags().BoolVar(&backupListJSON, "json", false, "machine-readable output")
	backupCmd.AddCommand(backupInitCmd, backupCreateCmd, backupListCmd)
	rootCmd.AddCommand(backupCmd)
}
