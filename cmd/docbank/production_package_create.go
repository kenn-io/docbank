package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/production"
)

func writeProductionPackageEvidence(cmd *cobra.Command, evidence production.PackageEvidenceReceipt,
	asJSON bool) error {
	if asJSON {
		return writeCLIJSON(cmd.OutOrStdout(), evidence)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "package %s job=%s archive=%s sha256=%s\n",
		evidence.ID, evidence.JobID, evidence.ArchiveSHA256, evidence.SHA256)
	if err != nil {
		return fmt.Errorf("writing production package evidence: %w", err)
	}
	return nil
}

func newProductionPackageCreateCommand() *cobra.Command {
	return newProductionPackageCreateCommandWithEnsure(daemonconn.Ensure)
}

func newProductionPackageCreateCommandWithEnsure(
	ensure func(context.Context) (*daemonconn.Connection, error)) *cobra.Command {
	var operationID, profileID string
	var maxVolumeBytes int64
	var maxVolumeDocuments int
	var asJSON bool
	cmd := &cobra.Command{Use: "create <published-job-id>",
		Short: "Build and retain a verified recipient package from published artifacts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !daemonconn.IsCanonicalUUIDv4(args[0]) || !daemonconn.IsCanonicalUUIDv4(operationID) ||
				profileID == "" || maxVolumeBytes < 1 || maxVolumeBytes > 50<<30 ||
				maxVolumeDocuments < 1 || maxVolumeDocuments > 100_000 {
				return usageError(errors.New("published job and operation UUIDv4, profile and bounded volume limits are required"))
			}
			connection, err := ensure(cmd.Context())
			if err != nil {
				return err
			}
			evidence, err := connection.CreateProductionPackage(cmd.Context(), args[0],
				api.ProductionPackageCreateRequest{OperationID: operationID, ProfileID: profileID,
					MaxVolumeBytes: maxVolumeBytes, MaxVolumeDocuments: maxVolumeDocuments})
			if err != nil {
				return err
			}
			return writeProductionPackageEvidence(cmd, evidence, asJSON)
		}}
	cmd.Flags().StringVar(&operationID, "operation-id", "", "stable UUID for replay-safe package creation")
	cmd.Flags().StringVar(&profileID, "profile", "", "recipient export profile")
	cmd.Flags().Int64Var(&maxVolumeBytes, "max-volume-bytes", 1<<30, "maximum bytes per recipient volume")
	cmd.Flags().IntVar(&maxVolumeDocuments, "max-volume-documents", 1000,
		"maximum documents per recipient volume")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable package evidence JSON")
	return cmd
}

func newProductionPackageShowCommand() *cobra.Command {
	return newProductionPackageShowCommandWithEnsure(daemonconn.Ensure)
}

func newProductionPackageShowCommandWithEnsure(
	ensure func(context.Context) (*daemonconn.Connection, error)) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "show <published-job-id> <operation-id>",
		Short: "Read exact retained package evidence", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !daemonconn.IsCanonicalUUIDv4(args[0]) || !daemonconn.IsCanonicalUUIDv4(args[1]) {
				return usageError(errors.New("published job and operation IDs must be UUIDv4"))
			}
			connection, err := ensure(cmd.Context())
			if err != nil {
				return err
			}
			evidence, err := connection.ProductionPackage(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return writeProductionPackageEvidence(cmd, evidence, asJSON)
		}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable package evidence JSON")
	return cmd
}

func init() {
	productionPackageCmd.AddCommand(newProductionPackageCreateCommand(),
		newProductionPackageShowCommand())
}
