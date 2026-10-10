package mcp

import (
	"context"

	"go.kenn.io/docbank/internal/daemonconn"
)

var reportReleaseTool = toolDefinition{
	name: "release_report", title: "Release report", write: true, destructive: true,
	description: "Discard a live report's packet and date-review evidence, " +
		"even when review is pending. " +
		"History, saved files and published child reports remain. " +
		"An active download returns report_retained; retry deliberately after it closes. " +
		"Never retry automatically after a lost reply. Inspect get_report_summary for this ID: " +
		"success means it remains; report_unavailable is inconclusive because reporting may be down. " +
		"History cannot confirm release. A deliberate retry uses the same ID and creates no handle.",
	schemas: releaseReportSchemas,
}

type reportReleaseOutput struct {
	privateCache

	ReportID string `json:"report_id"`
	Released bool   `json:"released"`
}

func releaseReportSchemas() (schema, schema) {
	released := booleanSchema()
	released["const"] = true
	return rootObjectSchema(schema{"report_id": reportIDSchema()}, "report_id"),
		rootObjectSchema(withPrivateCache(schema{
			"report_id": reportIDSchema(), "released": released,
		}), cacheRequired("report_id", "released")...)
}

func (r *reportTools) release(ctx context.Context, raw []byte) (reportReleaseOutput, error) {
	var input struct {
		ReportID string `json:"report_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return reportReleaseOutput{}, err
	}
	err := daemonProcessingStartVoid(ctx, r.lease, func(c *daemonconn.Connection) error {
		return c.ReleaseTermReport(ctx, input.ReportID)
	})
	if err != nil {
		return reportReleaseOutput{}, err
	}
	return reportReleaseOutput{
		privateCache: newPrivateCache(), ReportID: input.ReportID, Released: true,
	}, nil
}
