package cmd

import (
	"context"
	"fmt"
	"sort"

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
		untracked, untrackedBytes, err := untrackedBlobFiles(cmd.Context(), v)
		if err != nil {
			return err
		}
		bytes := untrackedBytes
		for _, c := range candidates {
			bytes += c.Size
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"%d candidate blob(s), %d untracked file(s), %d byte(s) reclaimable\n",
			len(candidates), len(untracked), bytes)
		if !gcRun || len(candidates)+len(untracked) == 0 {
			if !gcRun && len(candidates)+len(untracked) > 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "dry run — pass --run to delete")
			}
			return nil
		}

		// Untracked files have no rows; removing the file is the whole job.
		for _, h := range untracked {
			if err := v.blobs.Remove(h); err != nil {
				return err
			}
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
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "reclaimed %d blob(s), %d byte(s)\n",
			len(hashes)+len(untracked), bytes)
		return nil
	},
}

// untrackedBlobFiles lists blob files that have no blobs row: a durable
// blob write whose metadata transaction failed (or a crash between the two)
// leaves a file no row-based query can see. Safe to compute and act on only
// under the exclusive vault lock — with writers running, a file could be
// mid-ingest between its Write and its row commit.
func untrackedBlobFiles(ctx context.Context, v *vault) ([]string, int64, error) {
	tracked, err := v.store.AllBlobs(ctx)
	if err != nil {
		return nil, 0, err
	}
	trackedSet := make(map[string]bool, len(tracked))
	for _, b := range tracked {
		trackedSet[b.Hash] = true
	}
	files, err := v.blobs.List()
	if err != nil {
		return nil, 0, err
	}
	var untracked []string
	var bytes int64
	for hash, size := range files {
		if !trackedSet[hash] {
			untracked = append(untracked, hash)
			bytes += size
		}
	}
	sort.Strings(untracked)
	return untracked, bytes, nil
}

func init() {
	gcCmd.Flags().BoolVar(&gcRun, "run", false, "actually delete (default is dry-run)")
	rootCmd.AddCommand(gcCmd)
}
