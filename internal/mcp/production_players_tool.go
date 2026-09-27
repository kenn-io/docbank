package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

const maxProductionPlayersInputBytes = 64 << 20

var errProductionPlayersOutcomeUnknown = errors.New("production players write outcome is unknown")

type productionPlayersOutput struct {
	privateCache

	SnapshotID  string `json:"snapshot_id"`
	Revision    int64  `json:"revision"`
	SHA256      string `json:"sha256"`
	PlayerCount int    `json:"player_count"`
}

func readPrivateProductionPlayersFile(path string, snapshotID string, revision int64) (
	[]documentproduction.Player, error) {
	if len(path) == 0 || len(path) > maxPathCharacters || !filepath.IsAbs(path) ||
		filepath.Clean(path) != path {
		return nil, invalidToolArgumentsError()
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxProductionPlayersInputBytes {
		return nil, invalidToolArgumentsError()
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, invalidToolArgumentsError()
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, invalidToolArgumentsError()
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxProductionPlayersInputBytes+1))
	if err != nil || len(raw) > maxProductionPlayersInputBytes {
		return nil, invalidToolArgumentsError()
	}
	var players []documentproduction.Player
	if err := json.Unmarshal(raw, &players, json.RejectUnknownMembers(true)); err != nil ||
		len(players) == 0 || len(players) > documentproduction.MaxPrivilegeRows {
		return nil, invalidToolArgumentsError()
	}
	if _, _, err := documentproduction.CanonicalPlayersSnapshot(documentproduction.PlayersSnapshot{
		Contract: documentproduction.PlayersSnapshotContractV1,
		ID:       snapshotID, Revision: revision, Players: players,
	}); err != nil {
		return nil, invalidToolArgumentsError()
	}
	return players, nil
}

func createProductionPlayersSnapshot(ctx context.Context, lease *daemonLease, raw []byte) (productionPlayersOutput, error) {
	if len(raw) > 8192 {
		return productionPlayersOutput{}, invalidToolArgumentsError()
	}
	var input struct {
		SnapshotID  string `json:"snapshot_id"`
		Revision    int64  `json:"revision"`
		OperationID string `json:"operation_id"`
		PlayersFile string `json:"players_file"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionPlayersOutput{}, err
	}
	if !validToolUUID(input.SnapshotID) || input.Revision < 1 || !validToolUUID(input.OperationID) {
		return productionPlayersOutput{}, invalidToolArgumentsError()
	}
	players, err := readPrivateProductionPlayersFile(input.PlayersFile, input.SnapshotID, input.Revision)
	if err != nil {
		return productionPlayersOutput{}, err
	}
	result, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (
		documentproduction.PlayersSnapshot, error) {
		return c.CreateProductionPlayersSnapshot(ctx, input.SnapshotID, input.Revision,
			api.ProductionPlayersSnapshotCreateRequest{OperationID: input.OperationID, Players: players})
	})
	if err != nil {
		if errors.Is(err, errProcessingOutcomeUnknown) {
			return productionPlayersOutput{}, errProductionPlayersOutcomeUnknown
		}
		return productionPlayersOutput{}, err
	}
	return productionPlayersOutput{privateCache: newPrivateCache(), SnapshotID: result.ID,
		Revision: result.Revision, SHA256: result.SHA256, PlayerCount: len(result.Players)}, nil
}

func createProductionPlayersSnapshotToolHandler(lease *daemonLease, validator *jsonschema.Resolved,
	logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := createProductionPlayersSnapshot(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, createProductionPlayersSnapshotToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
