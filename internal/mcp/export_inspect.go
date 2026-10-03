package mcp

import (
	"context"
	"uuid"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

var exportPlanTool = toolDefinition{
	name: "get_export_plan", title: "Get export plan",
	description: "Read a frozen export header by plan ID. Its expires_at is the admission deadline, " +
		"not the read-retention deadline. Reading extends neither. An expired plan needs a new " +
		"preview for a new export; inspect an existing job with get_export_status.",
	schemas: getExportPlanSchemas,
}

var exportProblemsTool = toolDefinition{
	name: "get_export_problems", title: "Get export problems",
	description: "Read up to 50 frozen unavailable-output details. Pass a nonzero next as after; " +
		"zero means the last page. Totals count problems, not documents. A page over 64 KiB fails; " +
		"HTTP has the same limit, and releasing jobs cannot shrink it.",
	schemas: getExportProblemsSchemas,
}

type exportInspectionInput struct {
	PlanID uuid.UUID `json:"plan_id"`
	After  int64     `json:"after"`
}

type exportProblemsOutput struct {
	privateCache

	Problems bundle.OutputProblems `json:"problems"`
}

func getExportPlan(ctx context.Context, lease *daemonLease, raw []byte) (exportPlanOutput, error) {
	var input exportInspectionInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportPlanOutput{}, err
	}
	plan, err := daemonRead(ctx, lease, func(
		ctx context.Context, c *daemonconn.Connection,
	) (*bundle.Plan, error) {
		return c.API().GetExportPlan(ctx, &apiclient.GetExportPlanRequestOptions{
			PathParams: &apiclient.GetExportPlanPath{ID: input.PlanID.String()},
		})
	})
	if err != nil {
		return exportPlanOutput{}, err
	}
	return exportPlanOutput{privateCache: newPrivateCache(), Plan: *plan}, nil
}

func getExportProblems(
	ctx context.Context, lease *daemonLease, raw []byte,
) (exportProblemsOutput, error) {
	var input exportInspectionInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return exportProblemsOutput{}, err
	}
	problems, err := daemonRead(ctx, lease, func(
		ctx context.Context, c *daemonconn.Connection,
	) (*bundle.OutputProblems, error) {
		return c.API().GetExportOutputProblems(ctx, &apiclient.GetExportOutputProblemsRequestOptions{
			PathParams: &apiclient.GetExportOutputProblemsPath{ID: input.PlanID.String()},
			Query:      &apiclient.GetExportOutputProblemsQuery{After: new(input.After)},
		})
	})
	if err != nil {
		return exportProblemsOutput{}, err
	}
	return exportProblemsOutput{privateCache: newPrivateCache(), Problems: *problems}, nil
}
