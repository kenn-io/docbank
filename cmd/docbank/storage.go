package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

var storageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Inspect and maintain physical blob storage",
}

var storageStatusJSON bool

var storageStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report loose and packed storage usage",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		status, err := c.StorageStatus(cmd.Context())
		if err != nil {
			return err
		}
		if storageStatusJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(status)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "loose: %d blob(s), %d byte(s)\n",
			status.LooseBlobs, status.LooseBytes)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"packed: %d live blob(s) in %d pack(s), %d stored byte(s), %d raw byte(s)\n",
			status.PackedBlobs, status.Packs, status.PackedStoredBytes, status.PackedRawBytes)
		if status.DeadPackedBytes > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "pending repack: %d stored byte(s)\n",
				status.DeadPackedBytes)
		}
		return nil
	},
}

var (
	storagePackMaxBytes int64
	storagePackJSON     bool
)

var storagePackCmd = &cobra.Command{
	Use:   "pack",
	Short: "Pack authorized loose blobs into immutable pack files",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		report, err := c.StoragePack(cmd.Context(), storagePackMaxBytes)
		if err != nil {
			return err
		}
		if storagePackJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(report)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "packed %d blob(s), %d raw byte(s), sealed %d pack(s)\n",
			report.BlobsPacked, report.BytesPacked, report.PacksSealed)
		if report.LooseSwept > 0 || report.LooseOrphansRemoved > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"removed %d redundant loose file(s) and %d orphan loose object(s)\n",
				report.LooseSwept, report.LooseOrphansRemoved)
		}
		if report.PacksAdopted+report.PacksRemoved+report.PacksQuarantined+
			report.PacksUnreadable+report.RecordsDropped > 0 || report.MappingsPruned > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"reconciled packs: %d adopted, %d removed, %d quarantined, %d unreadable; "+
					"%d record(s) dropped, %d mapping(s) pruned\n",
				report.PacksAdopted, report.PacksRemoved, report.PacksQuarantined,
				report.PacksUnreadable, report.RecordsDropped, report.MappingsPruned)
		}
		if report.BlobsMissing+report.BlobsCorrupt+report.BlobsDeferredOversized+
			report.PacksDeferredOversized > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"deferred: %d missing blob(s), %d corrupt blob(s), %d oversized blob(s), %d oversized pack(s)\n",
				report.BlobsMissing, report.BlobsCorrupt, report.BlobsDeferredOversized,
				report.PacksDeferredOversized)
		}
		if report.LooseOrphanSweepSuppressed {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "orphan loose sweep suppressed: reference inventory was incomplete")
		}
		if report.BudgetExhausted {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "byte budget exhausted; rerun if loose blobs remain")
		}
		return nil
	},
}

func init() {
	storageStatusCmd.Flags().BoolVar(&storageStatusJSON, "json", false, "machine-readable output")
	storagePackCmd.Flags().Int64Var(&storagePackMaxBytes, "max-bytes", 0,
		"soft raw-byte work budget (0 is unlimited)")
	storagePackCmd.Flags().BoolVar(&storagePackJSON, "json", false, "machine-readable output")
	storageCmd.AddCommand(storageStatusCmd, storagePackCmd)
	rootCmd.AddCommand(storageCmd)
}
