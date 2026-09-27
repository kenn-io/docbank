package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/production"
)

const maxProductionReproductionFileBytes = 1 << 20

var productionReproductionCmd = &cobra.Command{
	Use: "reproduction", Short: "Create and read packages from published production artifacts",
}

func writeProductionReproduction(cmd *cobra.Command, receipt documentproduction.ReproductionReceipt,
	asJSON bool) error {
	if asJSON {
		return writeCLIJSON(cmd.OutOrStdout(), receipt)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "reproduction %s original=%s package_qc=%s sha256=%s\n",
		receipt.ID, receipt.OriginalProductionReceiptSHA256, receipt.PackageQCSHA256, receipt.SHA256)
	if err != nil {
		return fmt.Errorf("writing production reproduction: %w", err)
	}
	return nil
}

func readProductionReproductionInput(path string) (api.ProductionReproductionCreateRequest, error) {
	file, err := os.Open(path)
	if err != nil {
		return api.ProductionReproductionCreateRequest{}, fmt.Errorf("opening reproduction file: %w", err)
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxProductionReproductionFileBytes+1))
	if err != nil {
		return api.ProductionReproductionCreateRequest{}, fmt.Errorf("reading reproduction file: %w", err)
	}
	if len(raw) > maxProductionReproductionFileBytes {
		return api.ProductionReproductionCreateRequest{}, usageError(errors.New("reproduction file exceeds 1 MiB"))
	}
	var input api.ProductionReproductionCreateRequest
	if err := json.Unmarshal(raw, &input, json.RejectUnknownMembers(true)); err != nil {
		return api.ProductionReproductionCreateRequest{}, usageError(fmt.Errorf("invalid reproduction JSON: %w", err))
	}
	policySHA256, err := production.PackageDeliveryPolicySHA256(
		production.PackageDeliveryPolicy(input.DeliveryPolicy))
	if err != nil || input.Request.DeliveryPolicySHA256 != "" &&
		policySHA256 != input.Request.DeliveryPolicySHA256 {
		return api.ProductionReproductionCreateRequest{}, usageError(errors.New("delivery policy disagrees with reproduction request"))
	}
	input.Request.DeliveryPolicySHA256 = policySHA256
	if _, _, err := documentproduction.CanonicalReproductionRequest(input.Request); err != nil {
		return api.ProductionReproductionCreateRequest{}, usageError(fmt.Errorf("invalid reproduction request: %w", err))
	}
	if input.ProfileID == "" || input.MaxVolumeBytes < 1 || input.MaxVolumeBytes > 50<<30 ||
		input.MaxVolumeDocuments < 1 || input.MaxVolumeDocuments > 100_000 {
		return api.ProductionReproductionCreateRequest{}, usageError(errors.New("invalid reproduction package profile or limits"))
	}
	return input, nil
}

func newProductionReproductionCreateCommand() *cobra.Command {
	return newProductionReproductionCreateCommandWithEnsure(daemonconn.Ensure)
}

func newProductionReproductionCreateCommandWithEnsure(
	ensure func(context.Context) (*daemonconn.Connection, error)) *cobra.Command {
	var file string
	var asJSON bool
	cmd := &cobra.Command{Use: "create <published-job-id>",
		Short: "Build and retain a verified package from published artifacts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !daemonconn.IsCanonicalUUIDv4(args[0]) || file == "" {
				return usageError(errors.New("published job UUID and --file are required"))
			}
			input, err := readProductionReproductionInput(file)
			if err != nil {
				return err
			}
			connection, err := ensure(cmd.Context())
			if err != nil {
				return err
			}
			receipt, err := connection.CreateProductionReproduction(cmd.Context(), args[0], input)
			if err != nil {
				return err
			}
			return writeProductionReproduction(cmd, receipt, asJSON)
		}}
	cmd.Flags().StringVar(&file, "file", "", "bounded JSON request and delivery policy (policy digest may be omitted)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable reproduction receipt JSON")
	return cmd
}

func newProductionReproductionShowCommand() *cobra.Command {
	return newProductionReproductionShowCommandWithEnsure(daemonconn.Ensure)
}

func newProductionReproductionShowCommandWithEnsure(
	ensure func(context.Context) (*daemonconn.Connection, error)) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "show <published-job-id> <operation-id>",
		Short: "Read an exact verified reproduction receipt", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !daemonconn.IsCanonicalUUIDv4(args[0]) || !daemonconn.IsCanonicalUUIDv4(args[1]) {
				return usageError(errors.New("published job and operation IDs must be UUIDv4"))
			}
			connection, err := ensure(cmd.Context())
			if err != nil {
				return err
			}
			receipt, err := connection.ProductionReproduction(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return writeProductionReproduction(cmd, receipt, asJSON)
		}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable reproduction receipt JSON")
	return cmd
}

func init() {
	productionReproductionCmd.AddCommand(newProductionReproductionCreateCommand(),
		newProductionReproductionShowCommand())
	productionCmd.AddCommand(productionReproductionCmd)
}
