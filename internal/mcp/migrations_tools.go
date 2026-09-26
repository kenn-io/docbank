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
	privateCache
	migrationRunSummary

	OwnerMapPath string `json:"owner_map_path,omitzero"`
}

type migrationRunPageToolOutput struct {
	privateCache

	Total int                   `json:"total"`
	Items []migrationRunSummary `json:"items"`
}

type migrationReportSummary struct {
	Source                api.MigrationSource   `json:"source"`
	Schema                api.MigrationSchema   `json:"schema"`
	Counts                api.MigrationCounts   `json:"counts"`
	Capacity              api.MigrationCapacity `json:"capacity"`
	CreatedAt             string                `json:"created_at"`
	VectorGenerationCount int                   `json:"vector_generation_count"`
}

type migrationOwnerMapSummary struct {
	Source     api.MigrationSource `json:"source"`
	EntryCount int                 `json:"entry_count"`
}

type migrationRunSummary struct {
	ID        string                   `json:"id"`
	Source    api.MigrationSource      `json:"source"`
	CreatedAt string                   `json:"created_at"`
	Report    migrationReportSummary   `json:"report"`
	OwnerMap  migrationOwnerMapSummary `json:"owner_map"`
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
	return migrationRunToolOutput{
		migrationRunSummary: projectMigrationRun(run),
		OwnerMapPath:        run.OwnerMapPath,
		privateCache:        newPrivateCache(),
	}, nil
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
	items := make([]migrationRunSummary, 0, len(page.Items))
	for _, run := range page.Items {
		items = append(items, projectMigrationRun(run))
	}
	return migrationRunPageToolOutput{Total: page.Total, Items: items, privateCache: newPrivateCache()}, nil
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
	return migrationRunToolOutput{migrationRunSummary: projectMigrationRun(run), privateCache: newPrivateCache()}, nil
}

func projectMigrationRun(run api.MigrationRun) migrationRunSummary {
	return migrationRunSummary{
		ID:        run.ID,
		Source:    run.Source,
		CreatedAt: run.CreatedAt,
		Report: migrationReportSummary{
			Source:                run.Report.Source,
			Schema:                run.Report.Schema,
			Counts:                run.Report.Counts,
			Capacity:              run.Report.Capacity,
			CreatedAt:             run.Report.CreatedAt,
			VectorGenerationCount: len(run.Report.Vectors),
		},
		OwnerMap: migrationOwnerMapSummary{
			Source:     run.OwnerMap.Source,
			EntryCount: len(run.OwnerMap.Entries),
		},
	}
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
