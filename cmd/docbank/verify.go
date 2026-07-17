package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
)

var verifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Validate metadata and re-hash every stored blob",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		rep, err := c.Verify(cmd.Context())
		if err != nil {
			return err
		}
		for _, p := range rep.Problems {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", p.Problem, p.Hash)
		}
		for _, problem := range rep.MetadataProblems {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "metadata: %s\n", problem)
		}
		problemCount := len(rep.Problems) + len(rep.MetadataProblems)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d blob(s) ok, %d problem(s)\n",
			rep.OK, problemCount)
		if problemCount > 0 {
			return fmt.Errorf("verify found %d problem(s)", problemCount)
		}
		return nil
	},
}

func init() { rootCmd.AddCommand(verifyCmd) }
