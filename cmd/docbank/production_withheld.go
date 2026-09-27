package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

const maxProductionWithheldFileBytes = 64 << 20

var productionWithheldCmd = &cobra.Command{Use: "withheld-selection",
	Short: "Record explicit version-pinned withheld members"}

func readProductionWithheldFile(path string) (api.ProductionWithheldSelectionCreateRequest, error) {
	reader, err := os.Open(path)
	if err != nil {
		return api.ProductionWithheldSelectionCreateRequest{}, fmt.Errorf("opening withheld selection file: %w", err)
	}
	defer func() { _ = reader.Close() }()
	raw, err := io.ReadAll(io.LimitReader(reader, maxProductionWithheldFileBytes+1))
	if err != nil {
		return api.ProductionWithheldSelectionCreateRequest{}, fmt.Errorf("reading withheld selection file: %w", err)
	}
	if len(raw) > maxProductionWithheldFileBytes {
		return api.ProductionWithheldSelectionCreateRequest{}, usageError(errors.New("withheld selection file exceeds 64 MiB"))
	}
	var request api.ProductionWithheldSelectionCreateRequest
	if err := json.Unmarshal(raw, &request, json.RejectUnknownMembers(true)); err != nil {
		return api.ProductionWithheldSelectionCreateRequest{}, usageError(errors.New("invalid withheld selection JSON"))
	}
	return request, nil
}

func newProductionWithheldCreateCommand() *cobra.Command {
	return newProductionWithheldCreateCommandWithEnsure(daemonconn.Ensure)
}

func newProductionWithheldCreateCommandWithEnsure(
	ensure func(context.Context) (*daemonconn.Connection, error)) *cobra.Command {
	var file string
	var asJSON bool
	cmd := &cobra.Command{Use: "create <set-id> <revision>",
		Short: "Record a withheld selection from sealed production membership", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			revision, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || revision < 1 || !daemonconn.IsCanonicalUUIDv4(args[0]) || file == "" {
				return usageError(errors.New("set UUIDv4, positive revision and --file are required"))
			}
			request, err := readProductionWithheldFile(file)
			if err != nil {
				return err
			}
			if !daemonconn.IsCanonicalUUIDv4(request.OperationID) {
				return usageError(errors.New("withheld selection operation ID must be UUIDv4"))
			}
			if _, _, err := documentproduction.CanonicalWithheldSelection(documentproduction.WithheldSelection{
				Contract: documentproduction.WithheldSelectionContractV1,
				ID:       request.SelectionID, SetID: args[0], Revision: revision,
				PolicySHA256: request.PolicySHA256, Members: request.Members,
			}); err != nil {
				return usageError(errors.New("invalid withheld selection"))
			}
			connection, err := ensure(cmd.Context())
			if err != nil {
				return err
			}
			created, err := connection.CreateProductionWithheldSelection(cmd.Context(), args[0], revision, request)
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), created)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s set=%s revision=%d sha256=%s\n",
				created.ID, created.SetID, created.Revision, created.SHA256)
			if err != nil {
				return fmt.Errorf("writing withheld selection: %w", err)
			}
			return nil
		}}
	cmd.Flags().StringVar(&file, "file", "", "JSON file with operation ID, selection ID, policy digest and withheld members")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable withheld selection JSON")
	return cmd
}

func init() {
	productionWithheldCmd.AddCommand(newProductionWithheldCreateCommand())
	productionCmd.AddCommand(productionWithheldCmd)
}
