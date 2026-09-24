package mcp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

type exportSourceOutput struct {
	privateCache

	SourceID   string `json:"source_id"`
	MemberHash string `json:"member_hash"`
	Total      int    `json:"total"`
	State      string `json:"state"`
}

type exportPlanOutput struct {
	privateCache

	PlanID      string `json:"plan_id"`
	SourceID    string `json:"source_id"`
	Fingerprint string `json:"fingerprint"`
	Total       int    `json:"total"`
}

type exportPreviewOutput struct {
	privateCache

	PlanID      string               `json:"plan_id"`
	Fingerprint string               `json:"fingerprint"`
	MemberHash  string               `json:"member_hash"`
	Total       int                  `json:"total"`
	Roles       []bundle.RoleSummary `json:"roles"`
}

type exportJobOutput struct {
	privateCache

	JobID       string          `json:"job_id"`
	PlanID      string          `json:"plan_id"`
	Fingerprint string          `json:"fingerprint"`
	State       string          `json:"state"`
	Sequence    int64           `json:"sequence"`
	Receipt     *bundle.Receipt `json:"receipt,omitzero"`
}

type exportCancelOutput struct {
	privateCache

	JobID    string `json:"job_id"`
	Accepted bool   `json:"accepted"`
}

func exportToolHandler(lease *daemonLease, name string, validator *jsonschema.Resolved,
	logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		var output any
		var err error
		switch name {
		case "create_export_source":
			output, err = createMCPExportSource(ctx, lease, request.Params.Arguments)
		case "create_export_plan":
			output, err = createMCPExportPlan(ctx, lease, request.Params.Arguments)
		case "preview_export_plan":
			output, err = previewMCPExportPlan(ctx, lease, request.Params.Arguments)
		case "start_export_job":
			output, err = startMCPExportJob(ctx, lease, request.Params.Arguments)
		case "get_export_job":
			output, err = getMCPExportJob(ctx, lease, request.Params.Arguments)
		case "cancel_export_job":
			output, err = cancelMCPExportJob(ctx, lease, request.Params.Arguments)
		default:
			err = errors.New("unknown export tool")
		}
		if err == nil {
			var result *sdkmcp.CallToolResult
			result, err = boundedToolSuccess(validator, output, nil)
			if err == nil {
				return result, nil
			}
		}
		logOperationError(logger, name, err)
		if domain, ok := domainToolError(err); ok {
			return domain, nil
		}
		return nil, sanitizedRPCError(err)
	}
}

func createMCPExportSource(ctx context.Context, lease *daemonLease, raw []byte) (exportSourceOutput, error) {
	var input struct {
		OperationID string          `json:"operation_id"`
		Members     []bundle.Member `json:"members"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportSourceOutput{}, err
	}
	if !daemonconn.IsCanonicalUUIDv4(input.OperationID) || len(input.Members) == 0 || len(input.Members) > maxMCPExportMembers {
		return exportSourceOutput{}, invalidToolArgumentsError()
	}
	source, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*bundle.Source, error) {
		return c.API().CreateExportSource(ctx, &apiclient.CreateExportSourceRequestOptions{
			Body: &bundle.SourceRequest{OperationID: input.OperationID, Kind: "explicit", Members: input.Members},
		})
	})
	if err != nil {
		return exportSourceOutput{}, err
	}
	if source == nil || source.ID != input.OperationID || source.Kind != "explicit" || source.Total != len(input.Members) {
		return exportSourceOutput{}, errors.New("export source response does not bind selected documents")
	}
	return exportSourceOutput{privateCache: newPrivateCache(), SourceID: source.ID,
		MemberHash: source.MemberHash, Total: source.Total, State: source.State}, nil
}

func createMCPExportPlan(ctx context.Context, lease *daemonLease, raw []byte) (exportPlanOutput, error) {
	var input bundle.PlanRequest
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportPlanOutput{}, err
	}
	if !daemonconn.IsCanonicalUUIDv4(input.OperationID) || !daemonconn.IsCanonicalUUIDv4(input.SourceID) ||
		len(input.Roles) == 0 || len(input.Roles) > 8 {
		return exportPlanOutput{}, invalidToolArgumentsError()
	}
	plan, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*bundle.Plan, error) {
		return c.API().CreateExportPlan(ctx, &apiclient.CreateExportPlanRequestOptions{Body: &input})
	})
	if err != nil {
		return exportPlanOutput{}, err
	}
	if plan == nil || plan.ID != input.OperationID || plan.Source.ID != input.SourceID ||
		plan.Source.MemberHash != input.MemberHash || plan.Total < 1 || plan.Total > maxMCPExportMembers {
		return exportPlanOutput{}, errors.New("export plan response does not bind frozen source")
	}
	return exportPlanOutput{privateCache: newPrivateCache(), PlanID: plan.ID, SourceID: plan.Source.ID,
		Fingerprint: plan.Fingerprint, Total: plan.Total}, nil
}

func previewMCPExportPlan(ctx context.Context, lease *daemonLease, raw []byte) (exportPreviewOutput, error) {
	var input struct {
		PlanID string `json:"plan_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportPreviewOutput{}, err
	}
	if !daemonconn.IsCanonicalUUIDv4(input.PlanID) {
		return exportPreviewOutput{}, invalidToolArgumentsError()
	}
	preview, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*bundle.PlanPreview, error) {
		return c.API().GetExportPlanPreview(ctx, &apiclient.GetExportPlanPreviewRequestOptions{
			PathParams: &apiclient.GetExportPlanPreviewPath{ID: input.PlanID},
		})
	})
	if err != nil {
		return exportPreviewOutput{}, err
	}
	if preview == nil || preview.PlanID != input.PlanID || preview.Total < 1 || preview.Total > maxMCPExportMembers || len(preview.Roles) > 8 {
		return exportPreviewOutput{}, errors.New("export preview response does not bind plan")
	}
	return exportPreviewOutput{privateCache: newPrivateCache(), PlanID: preview.PlanID,
		Fingerprint: preview.Fingerprint, MemberHash: preview.MemberHash,
		Total: preview.Total, Roles: preview.Roles}, nil
}

func startMCPExportJob(ctx context.Context, lease *daemonLease, raw []byte) (exportJobOutput, error) {
	var input bundle.JobRequest
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportJobOutput{}, err
	}
	if !daemonconn.IsCanonicalUUIDv4(input.OperationID) || !daemonconn.IsCanonicalUUIDv4(input.PlanID) {
		return exportJobOutput{}, invalidToolArgumentsError()
	}
	job, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*bundle.ExportJob, error) {
		return c.API().CreateExportJob(ctx, &apiclient.CreateExportJobRequestOptions{Body: &input})
	})
	if err != nil {
		return exportJobOutput{}, err
	}
	if job == nil || job.ID != input.OperationID || job.PlanID != input.PlanID || job.Fingerprint != input.Fingerprint {
		return exportJobOutput{}, errors.New("export job response does not bind plan")
	}
	return mcpExportJob(*job), nil
}

func getMCPExportJob(ctx context.Context, lease *daemonLease, raw []byte) (exportJobOutput, error) {
	var input struct {
		JobID string `json:"job_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportJobOutput{}, err
	}
	if !daemonconn.IsCanonicalUUIDv4(input.JobID) {
		return exportJobOutput{}, invalidToolArgumentsError()
	}
	job, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*bundle.ExportJob, error) {
		return c.API().GetExportJob(ctx, &apiclient.GetExportJobRequestOptions{
			PathParams: &apiclient.GetExportJobPath{ID: input.JobID},
		})
	})
	if err != nil {
		return exportJobOutput{}, err
	}
	if job == nil || job.ID != input.JobID {
		return exportJobOutput{}, errors.New("export job response does not bind requested ID")
	}
	return mcpExportJob(*job), nil
}

func mcpExportJob(job bundle.ExportJob) exportJobOutput {
	return exportJobOutput{privateCache: newPrivateCache(), JobID: job.ID, PlanID: job.PlanID,
		Fingerprint: job.Fingerprint, State: job.State, Sequence: job.Sequence,
		Receipt: job.Receipt}
}

func cancelMCPExportJob(ctx context.Context, lease *daemonLease, raw []byte) (exportCancelOutput, error) {
	var input struct {
		JobID string `json:"job_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportCancelOutput{}, err
	}
	if !daemonconn.IsCanonicalUUIDv4(input.JobID) {
		return exportCancelOutput{}, invalidToolArgumentsError()
	}
	_, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*struct{}, error) {
		return c.API().CancelExportJob(ctx, &apiclient.CancelExportJobRequestOptions{
			PathParams: &apiclient.CancelExportJobPath{ID: input.JobID},
		})
	})
	if err != nil {
		return exportCancelOutput{}, err
	}
	return exportCancelOutput{privateCache: newPrivateCache(), JobID: input.JobID, Accepted: true}, nil
}
