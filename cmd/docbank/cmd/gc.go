package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var gcRun bool

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Reclaim unreachable blobs (dry-run unless --run)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Exclusive: no concurrent ingest may dedup against a file gc
		// is about to delete (see store.UnreachableBlobs docs).
		v, err := openVaultExclusive()
		if err != nil {
			return err
		}
		defer func() { _ = v.close() }()

		candidates, err := v.store.UnreachableBlobs(cmd.Context())
		if err != nil {
			return err
		}
		var bytes int64
		for _, c := range candidates {
			bytes += c.Size
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d candidate blob(s), %d byte(s) reclaimable\n",
			len(candidates), bytes)
		if !gcRun || len(candidates) == 0 {
			if !gcRun && len(candidates) > 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "dry run — pass --run to delete")
			}
			return nil
		}

		// Files first, rows second: a crash in between leaves rows without
		// files, which the next gc run reconciles (Remove tolerates missing).
		hashes := make([]string, 0, len(candidates))
		for _, c := range candidates {
			if err := v.blobs.Remove(c.Hash); err != nil {
				return err
			}
			hashes = append(hashes, c.Hash)
		}
		if err := v.store.DeleteBlobRows(cmd.Context(), hashes); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "reclaimed %d blob(s), %d byte(s)\n", len(hashes), bytes)
		return nil
	},
}

func init() {
	gcCmd.Flags().BoolVar(&gcRun, "run", false, "actually delete (default is dry-run)")
	rootCmd.AddCommand(gcCmd)
}
