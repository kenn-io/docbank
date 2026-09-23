package mcp

import (
	"context"
	"errors"
	"log/slog"
	"uuid"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

type getPackageImportInput struct {
	OperationID string `json:"operation_id"`
}

type startPackageImportInput struct {
	PreflightID       string `json:"preflight_id"`
	Into              string `json:"into"`
	Name              string `json:"name"`
	Party             string `json:"party"`
	OperationID       string `json:"operation_id"`
	AcceptPartial     bool   `json:"accept_partial"`
	IndexSuppliedText bool   `json:"index_supplied_text"`
}

type packageImportOutput struct {
	privateCache

	api.PackageImportJob
}

func getPackageImport(ctx context.Context, lease *daemonLease, raw []byte) (packageImportOutput, error) {
	var input getPackageImportInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return packageImportOutput{}, err
	}
	operationID, err := uuid.Parse(input.OperationID)
	if err != nil {
		return packageImportOutput{}, invalidToolArgumentsError()
	}
	job, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.PackageImportJob, error) {
		return c.API().ReadPackageImport(ctx, &apiclient.ReadPackageImportRequestOptions{
			PathParams: &apiclient.ReadPackageImportPath{OperationID: operationID},
		})
	})
	if err != nil {
		return packageImportOutput{}, err
	}
	return packageImportOutput{PackageImportJob: *job, privateCache: newPrivateCache()}, nil
}

func packageImportToolHandler(lease *daemonLease, validator *jsonschema.Resolved, logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		var input startPackageImportInput
		if err := decodeReadArguments(request.Params.Arguments, &input); err != nil {
			return nil, err
		}
		job, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*api.PackageImportJob, error) {
			return c.API().CreatePackageImport(ctx, &apiclient.CreatePackageImportRequestOptions{Body: &api.PackageImportRequest{
				PreflightID: input.PreflightID, Into: input.Into, Name: input.Name, Party: input.Party,
				OperationID: input.OperationID, AcceptPartial: input.AcceptPartial,
				IndexSuppliedText: input.IndexSuppliedText,
			}})
		})
		if err != nil {
			logOperationError(logger, packageImportToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		if job.OperationID != input.OperationID || job.PreflightID != input.PreflightID {
			return nil, sanitizedRPCError(errors.New("package import response does not bind the reviewed request"))
		}
		return boundedToolSuccess(validator, packageImportOutput{PackageImportJob: *job, privateCache: newPrivateCache()}, nil)
	}
}
