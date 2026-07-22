package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

var infoJSON bool

var infoCmd = &cobra.Command{
	Use:   "info",
	Short: "Identify the selected vault and summarize its contents",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		info, err := c.Info(cmd.Context())
		if err != nil {
			return err
		}
		if infoJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(info)
		}

		physicalBytes := info.Storage.LooseBytes + info.Storage.PackStoredBytes
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "vault: %s\n", info.VaultID)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "home: %q\n", info.VaultPath)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "live: %d file(s), %d folder(s)\n",
			info.LiveFiles, info.LiveDirectories)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "trash: %d node(s)\n", info.TrashedNodes)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "history: %d version(s), %d logical byte(s)\n",
			info.ContentVersions, info.LogicalVersionBytes)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "content: %d tracked blob(s), %d raw byte(s)\n",
			info.TrackedBlobs, info.TrackedBlobBytes)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "blob storage: %d stored byte(s); %d loose blob(s), %d pack(s)\n",
			physicalBytes, info.Storage.LooseBlobs, info.Storage.Packs)
		if info.Storage.DeadPackedBytes > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "pending repack: %d stored byte(s)\n",
				info.Storage.DeadPackedBytes)
		}
		return nil
	},
}

func init() {
	infoCmd.Flags().BoolVar(&infoJSON, "json", false, "machine-readable output")
	rootCmd.AddCommand(infoCmd)
}
