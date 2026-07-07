package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/ingest"
)

var addDest string

var addCmd = &cobra.Command{
	Use:   "add <path>...",
	Short: "Import files or directory trees into the vault",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dest := addDest
		addDest = "/inbox" // package-level flag vars persist across in-process Execute calls
		v, err := openVault()
		if err != nil {
			return err
		}
		defer func() { _ = v.close() }()

		ing := &ingest.Ingester{Store: v.store, Blobs: v.blobs}
		rep, err := ing.AddPaths(cmd.Context(), args, dest)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "added: %d  skipped: %d  failed: %d\n",
			rep.Added, rep.Skipped, len(rep.Failed))
		for _, f := range rep.Failed {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "failed: %s: %v\n", f.Path, f.Err)
		}
		if len(rep.Failed) > 0 {
			return fmt.Errorf("%d file(s) failed to import", len(rep.Failed))
		}
		return nil
	},
}

func init() {
	addCmd.Flags().StringVar(&addDest, "dest", "/inbox", "virtual destination directory")
	rootCmd.AddCommand(addCmd)
}
