package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

var gcRun bool

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Reclaim unreachable blobs (dry-run unless --run)",
	Args:  cobra.NoArgs,
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
			"%d candidate blob(s), %d untracked file(s), %d byte(s) reclaimable\n",
			rep.CandidateBlobs, rep.UntrackedFiles, rep.ReclaimableBytes)
		if !gcRun {
			if rep.CandidateBlobs+rep.UntrackedFiles > 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "dry run — pass --run to delete")
			}
			return nil
		}
		if rep.Removed > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "reclaimed %d blob(s), %d byte(s)\n",
				rep.Removed, rep.ReclaimableBytes)
		}
		return nil
	},
}

func init() {
	gcCmd.Flags().BoolVar(&gcRun, "run", false, "actually delete (default is dry-run)")
	rootCmd.AddCommand(gcCmd)
}
