package mcp

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type migrationRunToolOutput struct {
	api.MigrationRun
	privateCache
}

type migrationRunPageToolOutput struct {
	api.MigrationRunPage
	privateCache
}

func migrationInventoryToolHandler(lease *daemonLease, validator *jsonschema.Resolved, logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := inventoryFotobank(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, migrationInventoryToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}

func inventoryFotobank(ctx context.Context, lease *daemonLease, raw []byte) (migrationRunToolOutput, error) {
	var input api.FotobankInventoryRequest
	if err := decodeReadArguments(raw, &input); err != nil {
		return migrationRunToolOutput{}, err
	}
	if err := validateMigrationInventoryRequest(input); err != nil {
		return migrationRunToolOutput{}, err
	}
	run, err := daemonProcessingStart(ctx, lease, func(connection *daemonconn.Connection) (api.MigrationRun, error) {
		return connection.CreateFotobankInventory(ctx, input)
	})
	if err != nil {
		return migrationRunToolOutput{}, err
	}
	if run.OwnerMapPath != input.OwnerMapPath {
		return migrationRunToolOutput{}, errors.New("migration response did not bind the owner-map path")
	}
	return migrationRunToolOutput{MigrationRun: run, privateCache: newPrivateCache()}, nil
}

func listMigrationRuns(ctx context.Context, lease *daemonLease, raw []byte) (migrationRunPageToolOutput, error) {
	var input struct {
		Offset int `json:"offset"`
		Limit  int `json:"limit"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return migrationRunPageToolOutput{}, err
	}
	if input.Limit == 0 {
		input.Limit = 50
	}
	if input.Offset < 0 || input.Limit < 1 || input.Limit > 50 {
		return migrationRunPageToolOutput{}, errors.New("migration run page is outside the supported bound")
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, connection *daemonconn.Connection) (api.MigrationRunPage, error) {
		return connection.ListMigrationRuns(ctx, input.Offset, input.Limit)
	})
	if err != nil {
		return migrationRunPageToolOutput{}, err
	}
	if len(page.Items) > input.Limit || page.Total < len(page.Items) {
		return migrationRunPageToolOutput{}, errors.New("migration run page exceeded its requested bound")
	}
	if page.Items == nil {
		page.Items = []api.MigrationRun{}
	}
	return migrationRunPageToolOutput{MigrationRunPage: page, privateCache: newPrivateCache()}, nil
}

func showMigrationRun(ctx context.Context, lease *daemonLease, raw []byte) (migrationRunToolOutput, error) {
	var input struct {
		RunID string `json:"run_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return migrationRunToolOutput{}, err
	}
	run, err := daemonRead(ctx, lease, func(ctx context.Context, connection *daemonconn.Connection) (api.MigrationRun, error) {
		return connection.MigrationRun(ctx, input.RunID)
	})
	if err != nil {
		return migrationRunToolOutput{}, err
	}
	return migrationRunToolOutput{MigrationRun: run, privateCache: newPrivateCache()}, nil
}

func validateMigrationInventoryRequest(request api.FotobankInventoryRequest) error {
	install := request.CatalogPath != "" || request.VaultRoot != ""
	archive := request.ArchiveRoot != ""
	if install == archive {
		return errors.New("choose an install or an archive")
	}
	if install && (request.CatalogPath == "" || request.VaultRoot == "") {
		return errors.New("catalog_path and vault_root are both required for an install")
	}
	if request.OwnerMapPath == "" || !filepath.IsAbs(request.OwnerMapPath) {
		return errors.New("owner_map_path must be absolute")
	}
	return nil
}
