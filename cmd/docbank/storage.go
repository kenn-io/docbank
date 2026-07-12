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

func init() {
	storageStatusCmd.Flags().BoolVar(&storageStatusJSON, "json", false, "machine-readable output")
	storageCmd.AddCommand(storageStatusCmd)
	rootCmd.AddCommand(storageCmd)
}
