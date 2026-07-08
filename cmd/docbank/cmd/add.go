package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

var addDest string

var addCmd = &cobra.Command{
	Use:   "add <path>...",
	Short: "Import files or directory trees into the vault",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		abs := make([]string, len(args))
		for i, a := range args {
			p, err := filepath.Abs(a)
			if err != nil {
				return fmt.Errorf("resolving %q: %w", a, err)
			}
			abs[i] = p
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		rep, err := c.Ingest(cmd.Context(), abs, addDest)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "added: %d  skipped: %d  failed: %d\n",
			rep.Added, rep.Skipped, len(rep.Failed))
		for _, f := range rep.Failed {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "failed: %s: %s\n", f.Path, f.Error)
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
