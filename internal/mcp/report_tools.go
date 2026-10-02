package mcp

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/report"
)

var errReportOutcomeUnknown = errors.New("report outcome unknown")

var reportSummaryTool = toolDefinition{name: "get_report_summary", title: "Get report summary",
	description: "Inspect a frozen report's counts and coverage, " +
		"which may differ from current vault state.",
	schemas: getReportSummarySchemas}
var reportDatesTool = toolDefinition{name: "get_report_dates", title: "Get report dates",
	description: "Page through frozen date evidence. " +
		"Keep partial candidates and continue with the returned cursor.",
	schemas: getReportDatesSchemas}
var reportCreateTool = toolDefinition{name: "create_report", title: "Create report", write: true,
	description: "Capture a report for exact current document versions. " +
		"Review scope with the operator. " +
		"Does not process documents. A lost reply must not be retried automatically.",
	schemas: createReportSchemas}
var reportReviseTool = toolDefinition{name: "revise_report", title: "Revise report", write: true,
	description: "Apply operator-reviewed date choices to a frozen report. " +
		"Creates a child and consumes " +
		"another shared handle without extending expiry. Never retry automatically after a lost reply.",
	schemas: reviseReportSchemas}

type reportTools struct {
	lease  *daemonLease
	budget report.Budget
	logger *slog.Logger
}

type reportSummaryOutput struct {
	privateCache

	Summary report.Summary `json:"summary"`
}

type reportDatesOutput struct {
	privateCache

	ReportID string          `json:"report_id"`
	Page     report.DatePage `json:"page"`
}

type reportReceiptOutput struct {
	privateCache

	ReportID        string    `json:"report_id"`
	ParentID        string    `json:"parent_id,omitempty"`
	State           string    `json:"state"`
	ObservedAt      time.Time `json:"observed_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	UnresolvedDates int64     `json:"unresolved_dates"`
	BundleBytes     int64     `json:"bundle_bytes,omitzero"`
	BundleSHA256    string    `json:"bundle_sha256,omitempty"`
}

func reportReceipt(summary report.Summary) reportReceiptOutput {
	return reportReceiptOutput{privateCache: newPrivateCache(), ReportID: summary.ID,
		ParentID: summary.ParentID,
		State:    summary.State, ObservedAt: summary.ObservedAt, ExpiresAt: summary.ExpiresAt,
		UnresolvedDates: summary.UnresolvedDates, BundleBytes: summary.BundleBytes,
		BundleSHA256: summary.BundleSHA256}
}

func (r *reportTools) handler(name string, validator *jsonschema.Resolved) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		var output any
		var err error
		switch name {
		case reportSummaryTool.name:
			output, err = r.summary(ctx, request.Params.Arguments)
		case reportDatesTool.name:
			output, err = r.dates(ctx, request.Params.Arguments)
		case reportDownloadTool.name:
			output, err = r.download(ctx, request.Params.Arguments)
		case reportCreateTool.name:
			output, err = r.create(ctx, request.Params.Arguments)
		case reportReviseTool.name:
			output, err = r.revise(ctx, request.Params.Arguments)
		default:
			err = errors.New("unknown report tool")
		}
		if errors.Is(err, errProcessingOutcomeUnknown) {
			err = errReportOutcomeUnknown
		}
		if err == nil {
			var result *sdkmcp.CallToolResult
			result, err = boundedToolSuccess(validator, output, nil)
			if err == nil {
				return result, nil
			}
			err = reportResultError(name, err)
		}
		logOperationError(r.logger, name, err)
		if domain, ok := domainToolError(err); ok {
			return domain, nil
		}
		if invalid, ok := errors.AsType[*jsonrpc.Error](err); ok &&
			invalid.Code == jsonrpc.CodeInvalidParams {
			return nil, invalid
		}
		return nil, sanitizedRPCError(err)
	}
}

// Encoding happens after the daemon mutation; a rejected reply is not a rejected write.
func reportResultError(name string, err error) error {
	switch name {
	case reportCreateTool.name, reportReviseTool.name:
		return errReportOutcomeUnknown
	case reportSummaryTool.name, reportDatesTool.name:
		if errors.Is(err, errToolResultTooLarge) {
			return report.ErrReportLimit
		}
	}
	return err
}

func (r *reportTools) summary(ctx context.Context, raw []byte) (reportSummaryOutput, error) {
	var input struct {
		ReportID string `json:"report_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return reportSummaryOutput{}, err
	}
	summary, err := daemonRead(ctx, r.lease, func(
		ctx context.Context, c *daemonconn.Connection,
	) (report.Summary, error) {
		return c.GetTermReport(ctx, input.ReportID)
	})
	return reportSummaryOutput{privateCache: newPrivateCache(), Summary: summary}, err
}

func (r *reportTools) dates(ctx context.Context, raw []byte) (reportDatesOutput, error) {
	var input struct {
		ReportID string `json:"report_id"`
		Cursor   string `json:"cursor"`
		Limit    int    `json:"limit"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return reportDatesOutput{}, err
	}
	if len(input.Cursor) > 4096 {
		return reportDatesOutput{}, invalidToolArgumentsError()
	}
	page, err := daemonRead(ctx, r.lease, func(
		ctx context.Context, c *daemonconn.Connection,
	) (report.DatePage, error) {
		return c.TermReportDates(ctx, input.ReportID, report.DatePageRequest{
			Cursor: input.Cursor, Limit: input.Limit, MaxBytes: 256 << 10})
	})
	return reportDatesOutput{
		privateCache: newPrivateCache(), ReportID: input.ReportID, Page: page,
	}, err
}

func (r *reportTools) create(ctx context.Context, raw []byte) (reportReceiptOutput, error) {
	var input struct {
		Request report.Request `json:"request"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return reportReceiptOutput{}, err
	}
	if len(input.Request.Timezone) > 128 || len(input.Request.SourceTimezone) > 128 {
		return reportReceiptOutput{}, invalidToolArgumentsError()
	}
	request, err := report.NormalizeRequest(input.Request)
	if err != nil {
		return reportReceiptOutput{}, invalidToolArgumentsError()
	}
	summary, err := daemonProcessingStart(ctx, r.lease, func(
		c *daemonconn.Connection,
	) (report.Summary, error) {
		return c.CreateTermReport(ctx, request)
	})
	return reportReceipt(summary), err
}

func (r *reportTools) revise(ctx context.Context, raw []byte) (reportReceiptOutput, error) {
	var input struct {
		ReportID string              `json:"report_id"`
		Choices  []report.DateChoice `json:"choices"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return reportReceiptOutput{}, err
	}
	for _, choice := range input.Choices {
		if strings.TrimSpace(choice.Reason) == "" || len(choice.Reason) > 4096 ||
			len(choice.ReviewedTimezone) > 128 {
			return reportReceiptOutput{}, invalidToolArgumentsError()
		}
	}
	summary, err := daemonProcessingStart(ctx, r.lease, func(
		c *daemonconn.Connection,
	) (report.Summary, error) {
		return c.ReviseTermReport(ctx, input.ReportID, input.Choices)
	})
	return reportReceipt(summary), err
}
