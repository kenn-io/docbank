package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/production"
)

var productionSupplementCmd = &cobra.Command{
	Use: "supplement", Short: "Create and read exact production continuations",
}

func writeProductionSupplement(cmd *cobra.Command, record production.SupplementRecord, asJSON bool) error {
	if asJSON {
		return writeCLIJSON(cmd.OutOrStdout(), record)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s -> %s numbers %d-%d sha256=%s\n",
		record.ParentJobID, record.JobID, record.StartSequence, record.EndSequence, record.SHA256)
	if err != nil {
		return fmt.Errorf("writing production supplement: %w", err)
	}
	return nil
}

func newProductionSupplementCreateCommand() *cobra.Command {
	return newProductionSupplementCreateCommandWithEnsure(daemonconn.Ensure)
}

func newProductionSupplementCreateCommandWithEnsure(
	ensure func(context.Context) (*daemonconn.Connection, error)) *cobra.Command {
	var operationID, parentReceiptSHA256, preparedSHA256, preparedInputSHA256 string
	var asJSON bool
	cmd := &cobra.Command{Use: "create <published-parent-job-id> <prepared-child-job-id>",
		Short: "Reserve continuation numbers and link a prepared child to its published parent",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			request := production.SupplementRequest{
				OperationID: operationID, ParentJobID: args[0], JobID: args[1],
				ParentReceiptSHA256: parentReceiptSHA256,
				PreparedSHA256:      preparedSHA256,
				PreparedInputSHA256: preparedInputSHA256,
			}
			if _, err := production.SupplementRequestSHA256(request); err != nil {
				return usageError(fmt.Errorf("invalid supplement IDs or digests: %w", err))
			}
			connection, err := ensure(cmd.Context())
			if err != nil {
				return err
			}
			record, err := connection.CreateProductionSupplement(cmd.Context(), request)
			if err != nil {
				return err
			}
			return writeProductionSupplement(cmd, record, asJSON)
		}}
	cmd.Flags().StringVar(&operationID, "operation-id", "", "stable UUID for replay-safe supplement creation")
	cmd.Flags().StringVar(&parentReceiptSHA256, "parent-receipt-sha256", "", "published parent receipt SHA-256")
	cmd.Flags().StringVar(&preparedSHA256, "prepared-sha256", "", "prepared child revision SHA-256")
	cmd.Flags().StringVar(&preparedInputSHA256, "prepared-input-sha256", "", "prepared child input SHA-256")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable supplement record JSON")
	return cmd
}

func newProductionSupplementShowCommand() *cobra.Command {
	return newProductionSupplementShowCommandWithEnsure(daemonconn.Ensure)
}

func newProductionSupplementShowCommandWithEnsure(
	ensure func(context.Context) (*daemonconn.Connection, error)) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "show <operation-id>",
		Short: "Read an exact recorded production supplement", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			connection, err := ensure(cmd.Context())
			if err != nil {
				return err
			}
			record, err := connection.ProductionSupplement(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeProductionSupplement(cmd, record, asJSON)
		}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable supplement record JSON")
	return cmd
}

func init() {
	productionSupplementCmd.AddCommand(newProductionSupplementCreateCommand(),
		newProductionSupplementShowCommand())
	productionCmd.AddCommand(productionSupplementCmd)
}
