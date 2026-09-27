package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
)

var productionApprovalCmd = &cobra.Command{Use: "approval", Short: "Read public production approval status"}

func newProductionApprovalShowCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "show <approval-id>", Short: "Read a public approval grant and its lifecycle events",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			public, err := c.ProductionApproval(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), public)
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s sha256=%s actor=%s\n",
				public.Grant.ID, public.Grant.SHA256, public.Grant.Actor); err != nil {
				return fmt.Errorf("writing approval grant: %w", err)
			}
			for _, event := range public.Events {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s\n",
					event.ID, event.Kind, event.EffectiveAt); err != nil {
					return fmt.Errorf("writing approval event: %w", err)
				}
			}
			return nil
		}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func init() {
	productionApprovalCmd.AddCommand(newProductionApprovalShowCommand())
	productionCmd.AddCommand(productionApprovalCmd)
}
