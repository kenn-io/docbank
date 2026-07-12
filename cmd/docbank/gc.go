package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

var gcRun bool

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Reclaim unreachable blobs (dry-run unless --run)",
	Long: "Remove authority for blobs with no remaining references. With --run, loose " +
		"files are reclaimed immediately; packed payload becomes logically dead and " +
		"requires a separate storage repack to reclaim physical pack space.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		rep, err := c.GC(cmd.Context(), gcRun)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"%d candidate blob(s), %d untracked file(s), %d loose byte(s) reclaimable\n",
			rep.CandidateBlobs, rep.UntrackedFiles, rep.ReclaimableBytes)
		if rep.PendingPackedBlobs > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"%d packed blob(s), %d stored byte(s) pending repack\n",
				rep.PendingPackedBlobs, rep.PendingPackedBytes)
		}
		if !gcRun {
			if rep.CandidateBlobs+rep.UntrackedFiles > 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "dry run — pass --run to delete")
			}
			return nil
		}
		if rep.Removed > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"removed %d blob record(s); reclaimed %d loose file(s), %d byte(s)\n",
				rep.RemovedBlobs, rep.ReclaimedFiles, rep.ReclaimableBytes)
		}
		return nil
	},
}

func init() {
	gcCmd.Flags().BoolVar(&gcRun, "run", false, "actually delete (default is dry-run)")
	rootCmd.AddCommand(gcCmd)
}
