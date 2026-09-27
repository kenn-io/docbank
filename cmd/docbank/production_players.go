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

const maxProductionPlayersFileBytes = 64 << 20

var productionPlayersCmd = &cobra.Command{Use: "players", Short: "Record versioned people and aliases for privilege logs"}

func readProductionPlayersFile(path string) ([]documentproduction.Player, error) {
	reader, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening players file: %w", err)
	}
	defer func() { _ = reader.Close() }()
	raw, err := io.ReadAll(io.LimitReader(reader, maxProductionPlayersFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading players file: %w", err)
	}
	if len(raw) > maxProductionPlayersFileBytes {
		return nil, usageError(errors.New("players file exceeds 64 MiB"))
	}
	var players []documentproduction.Player
	if err := json.Unmarshal(raw, &players, json.RejectUnknownMembers(true)); err != nil ||
		len(players) == 0 || len(players) > documentproduction.MaxPrivilegeRows {
		return nil, usageError(errors.New("invalid players JSON"))
	}
	return players, nil
}

func newProductionPlayersCreateCommand() *cobra.Command {
	return newProductionPlayersCreateCommandWithEnsure(daemonconn.Ensure)
}

func newProductionPlayersCreateCommandWithEnsure(
	ensure func(context.Context) (*daemonconn.Connection, error)) *cobra.Command {
	var operationID, file string
	var asJSON bool
	cmd := &cobra.Command{Use: "create <snapshot-id> <revision>",
		Short: "Record an exact versioned player snapshot", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			revision, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || revision < 1 || !daemonconn.IsCanonicalUUIDv4(args[0]) ||
				!daemonconn.IsCanonicalUUIDv4(operationID) || file == "" {
				return usageError(errors.New("snapshot and operation UUIDv4, positive revision and --file are required"))
			}
			players, err := readProductionPlayersFile(file)
			if err != nil {
				return err
			}
			if _, _, err := documentproduction.CanonicalPlayersSnapshot(documentproduction.PlayersSnapshot{
				Contract: documentproduction.PlayersSnapshotContractV1,
				ID:       args[0], Revision: revision, Players: players,
			}); err != nil {
				return usageError(errors.New("invalid player snapshot"))
			}
			connection, err := ensure(cmd.Context())
			if err != nil {
				return err
			}
			created, err := connection.CreateProductionPlayersSnapshot(cmd.Context(), args[0], revision,
				api.ProductionPlayersSnapshotCreateRequest{OperationID: operationID, Players: players})
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), created)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d sha256=%s\n",
				created.ID, created.Revision, created.SHA256)
			if err != nil {
				return fmt.Errorf("writing player snapshot: %w", err)
			}
			return nil
		}}
	cmd.Flags().StringVar(&operationID, "operation-id", "", "stable UUID for replay-safe snapshot creation")
	cmd.Flags().StringVar(&file, "file", "", "JSON file containing the complete player array")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable player snapshot JSON")
	return cmd
}

func init() {
	productionPlayersCmd.AddCommand(newProductionPlayersCreateCommand())
	productionCmd.AddCommand(productionPlayersCmd)
}
