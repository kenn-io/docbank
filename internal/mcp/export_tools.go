package mcp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

var errExportOutcomeUnknown = errors.New("export outcome unknown; inspect the supplied identities")

var exportWriteToolDefinitions = []toolDefinition{
	{name: "download_export", title: "Download export", schemas: downloadExportSchemas,
		write: true, destructive: true,
		description: "Save a verified archive locally. Review the destination with the operator."},
	{name: "preview_export", title: "Preview export", schemas: previewExportSchemas, write: true,
		description: "Retain exact originals and a plan. Review membership with the operator."},
	{name: "start_export", title: "Start export", schemas: startExportSchemas, write: true,
		description: "Start a reviewed export plan using caller-owned operation and plan identities."},
	{name: "cancel_export", title: "Cancel export", schemas: cancelExportSchemas,
		write: true, destructive: true, description: "Cancel active export work by its job ID."},
	{name: "release_export", title: "Release export", schemas: releaseExportSchemas,
		write: true, destructive: true,
		description: "Discard a terminal job and its retained archive. Local downloaded files remain."},
}

type exportPreviewInput struct {
	SourceOperationID string          `json:"source_operation_id"`
	PlanOperationID   string          `json:"plan_operation_id"`
	Members           []bundle.Member `json:"members"`
}

type exportPlanOutput struct {
	privateCache

	Plan bundle.Plan `json:"plan"`
}

type exportJobOutput struct {
	privateCache

	Job bundle.Job `json:"job"`
}

type exportJobInput struct {
	JobID string `json:"job_id"`
}

type exportCancelOutput struct {
	privateCache

	JobID    string `json:"job_id"`
	Accepted bool   `json:"accepted"`
}

type exportReleaseOutput struct {
	privateCache

	JobID    string `json:"job_id"`
	Released bool   `json:"released"`
}

func exportToolHandler(
	lease *daemonLease, name string, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		var output any
		var err error
		switch name {
		case "preview_export":
			output, err = previewExport(ctx, lease, request.Params.Arguments)
		case "start_export":
			output, err = startExport(ctx, lease, request.Params.Arguments)
		case "get_export_status":
			output, err = getExportStatus(ctx, lease, request.Params.Arguments)
		case "cancel_export":
			output, err = cancelExport(ctx, lease, request.Params.Arguments)
		case "release_export":
			output, err = releaseExport(ctx, lease, request.Params.Arguments)
		case "download_export":
			output, err = downloadExport(ctx, lease, request.Params.Arguments, logger)
		default:
			err = errors.New("unknown export tool")
		}
		if err != nil {
			logOperationError(logger, name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			if invalid, ok := errors.AsType[*jsonrpc.Error](err); ok &&
				invalid.Code == jsonrpc.CodeInvalidParams {
				return nil, invalid
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}

func previewExport(ctx context.Context, lease *daemonLease, raw []byte) (exportPlanOutput, error) {
	var input exportPreviewInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportPlanOutput{}, err
	}
	type identity struct {
		node    int64
		version string
	}
	seen := make(map[identity]bool, len(input.Members))
	var total int64
	for _, member := range input.Members {
		key := identity{member.NodeID, member.VersionID}
		if seen[key] || member.Size > bundle.MaxRoleBytes-total {
			return exportPlanOutput{}, invalidToolArgumentsError()
		}
		seen[key] = true
		total += member.Size
	}
	plan, err := daemonProcessingStart(ctx, lease, func(
		c *daemonconn.Connection,
	) (*bundle.Plan, error) {
		source, err := c.API().CreateExportSource(ctx, &apiclient.CreateExportSourceRequestOptions{
			Body: &bundle.SourceRequest{
				OperationID: input.SourceOperationID, Kind: "explicit", Members: input.Members,
			},
		})
		if err != nil {
			return nil, err
		}
		return c.API().CreateExportPlan(ctx, &apiclient.CreateExportPlanRequestOptions{
			Body: &bundle.PlanRequest{
				OperationID: input.PlanOperationID, SourceID: source.ID, MemberHash: source.MemberHash,
				Roles: []bundle.RolePolicy{{Role: "original"}},
			},
		})
	})
	if errors.Is(err, errProcessingOutcomeUnknown) {
		return exportPlanOutput{}, errExportOutcomeUnknown
	}
	if err != nil {
		return exportPlanOutput{}, err
	}
	return exportPlanOutput{privateCache: newPrivateCache(), Plan: *plan}, nil
}

func startExport(ctx context.Context, lease *daemonLease, raw []byte) (exportJobOutput, error) {
	var input bundle.JobRequest
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportJobOutput{}, err
	}
	job, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*bundle.Job, error) {
		return c.API().CreateExportJob(ctx, &apiclient.CreateExportJobRequestOptions{Body: &input})
	})
	if errors.Is(err, errProcessingOutcomeUnknown) {
		return exportJobOutput{}, errExportOutcomeUnknown
	}
	if err != nil {
		return exportJobOutput{}, err
	}
	return exportJobOutput{privateCache: newPrivateCache(), Job: *job}, nil
}

func getExportStatus(ctx context.Context, lease *daemonLease, raw []byte) (exportJobOutput, error) {
	var input exportJobInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportJobOutput{}, err
	}
	job, err := daemonRead(ctx, lease, func(
		ctx context.Context, c *daemonconn.Connection,
	) (*bundle.Job, error) {
		return c.API().GetExportJob(ctx, &apiclient.GetExportJobRequestOptions{
			PathParams: &apiclient.GetExportJobPath{ID: input.JobID},
		})
	})
	if err != nil {
		return exportJobOutput{}, err
	}
	return exportJobOutput{privateCache: newPrivateCache(), Job: *job}, nil
}

func cancelExport(ctx context.Context, lease *daemonLease, raw []byte) (exportCancelOutput, error) {
	var input exportJobInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportCancelOutput{}, err
	}
	_, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*struct{}, error) {
		return c.API().CancelExportJob(ctx, &apiclient.CancelExportJobRequestOptions{
			PathParams: &apiclient.CancelExportJobPath{ID: input.JobID},
			Body:       &apiclient.CancelExportJobBody{},
		})
	})
	if errors.Is(err, errProcessingOutcomeUnknown) {
		return exportCancelOutput{}, errExportOutcomeUnknown
	}
	if err != nil {
		return exportCancelOutput{}, err
	}
	return exportCancelOutput{privateCache: newPrivateCache(), JobID: input.JobID, Accepted: true}, nil
}

func releaseExport(
	ctx context.Context, lease *daemonLease, raw []byte,
) (exportReleaseOutput, error) {
	var input exportJobInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportReleaseOutput{}, err
	}
	_, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*struct{}, error) {
		return c.API().ReleaseExportJob(ctx, &apiclient.ReleaseExportJobRequestOptions{
			PathParams: &apiclient.ReleaseExportJobPath{ID: input.JobID},
		})
	})
	if errors.Is(err, errProcessingOutcomeUnknown) {
		return exportReleaseOutput{}, errExportOutcomeUnknown
	}
	if err != nil {
		return exportReleaseOutput{}, err
	}
	return exportReleaseOutput{
		privateCache: newPrivateCache(), JobID: input.JobID, Released: true,
	}, nil
}
