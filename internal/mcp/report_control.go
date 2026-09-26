package mcp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

const (
	maxMCPReportDatePage    = 20
	maxMCPReportHistoryPage = 10
)

type reportSummaryOutput struct {
	privateCache
	report.Summary
}

type reportDatesOutput struct {
	privateCache
	report.DatePage
}

type reportHistoryOutput struct {
	privateCache
	store.TermReportHistoryPage
}

func reportIdentitySchema() schema {
	return objectSchema(schema{
		"node_id": integerSchema(1, 0), "version_id": stringSchema(128), "sha256": sha256Schema(), //nolint:goconst // Stable wire field is repeated across tools.
	}, "node_id", "version_id", "sha256")
}

func reportChoiceSchema() schema {
	return objectSchema(schema{
		"document": reportIdentitySchema(), "candidate_id": stringSchema(256),
		"evidence_sha256": sha256Schema(), "reason": stringSchema(4096),
		"action":        enumSchema("select", "interpret", "reclassify"),
		"reviewed_date": stringSchema(32), "reviewed_timezone": stringSchema(128),
		"reviewed_role": stringSchema(128),
	}, "document", "candidate_id", "evidence_sha256", "reason", "action")
}

func reportTermSchema() schema {
	return objectSchema(schema{
		"number": integerSchema(1, 0), "expression": stringSchema(8192),
		"syntax": enumSchema("simple", "advanced"),
		"dates":  objectSchema(schema{"start": stringSchema(10), "end": stringSchema(10)}, "start", "end"),
	}, "number", "expression", "syntax", "dates")
}

func reportRequestSchema() schema {
	return objectSchema(schema{
		"version": integerSchema(1, 2), "profile": stringSchema(128),
		"all_documents": booleanSchema(), "collection_ids": arraySchema(stringSchema(128), 50000),
		"selected_documents": arraySchema(reportIdentitySchema(), 50000),
		"timezone":           stringSchema(128), "source_timezone": stringSchema(128),
		"numeric_date_order": enumSchema("MDY", "DMY"),
		"coverage_mode":      enumSchema("strict", "available_only"),
		"terms":              arraySchema(reportTermSchema(), 128),
		"date_choices":       arraySchema(reportChoiceSchema(), 50000),
	}, "version", "all_documents", "timezone", "terms")
}

func reportSummaryProperties(states ...string) schema {
	counts := objectSchema(schema{
		"hits": integerSchema(0, 0), "hits_plus_family": integerSchema(0, 0),
		"unique_hits": integerSchema(0, 0), "unique_families": integerSchema(0, 0),
		"unique_hits_plus_family": integerSchema(0, 0),
	}, "hits", "hits_plus_family", "unique_hits", "unique_families", "unique_hits_plus_family")
	coverage := objectSchema(schema{
		"scoped": integerSchema(0, 0), "searchable": integerSchema(0, 0),
		"missing_text": integerSchema(0, 0), "incomplete_families": integerSchema(0, 0),
		"fallback_dates": integerSchema(0, 0), "warnings": arraySchema(stringSchema(512), 128),
	}, "scoped", "searchable", "missing_text", "incomplete_families", "fallback_dates")
	return schema{
		"id": reportIDSchema(), "parent_id": reportIDSchema(),
		"state":       enumSchema(states...),
		"observed_at": dateTimeSchema(), "expires_at": dateTimeSchema(),
		"terms": arraySchema(reportTermSchema(), 128), "counts": arraySchema(counts, 128),
		"coverage": coverage, "row_coverage": arraySchema(coverage, 128),
		"unresolved_dates": integerSchema(0, 50000),
		"csv_sha256":       sha256Schema(), "bundle_sha256": sha256Schema(),
		"csv_bytes": integerSchema(0, 512<<20), "bundle_bytes": integerSchema(0, 512<<20),
	}
}

func reportSummarySchema() schema {
	return rootObjectSchema(withPrivateCache(reportSummaryProperties("complete", "needs_review")),
		cacheRequired("id", "state", "observed_at", "expires_at", "terms", "coverage", "unresolved_dates")...)
}

func getReportSummarySchemas() (schema, schema) {
	return rootObjectSchema(schema{"report_id": reportIDSchema()}, "report_id"), reportSummarySchema()
}

func listReportHistorySchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		"offset": integerSchema(0, 100), "limit": integerSchema(1, maxMCPReportHistoryPage), //nolint:goconst // Published JSON field name.
	})
	summary := objectSchema(reportSummaryProperties("complete", "needs_review", "visibility_changed", "history_unavailable"),
		"id", "state", "observed_at", "expires_at", "terms", "coverage", "unresolved_dates")
	item := objectSchema(schema{"request": reportRequestSchema(), "summary": summary}, "request", "summary")
	output := rootObjectSchema(withPrivateCache(schema{
		"items": arraySchema(item, maxMCPReportHistoryPage), "total": integerSchema(0, 100), //nolint:goconst // Published JSON field name.
	}), cacheRequired("items", "total")...)
	return input, output
}

func createReportSchemas() (schema, schema) {
	return rootObjectSchema(schema{"request": reportRequestSchema()}, "request"), reportSummarySchema()
}

func reviseReportSchemas() (schema, schema) {
	return rootObjectSchema(schema{"report_id": reportIDSchema(),
		"choices": arraySchema(reportChoiceSchema(), 50000)}, "report_id", "choices"), reportSummarySchema()
}

func getReportDatesSchemas() (schema, schema) {
	input := rootObjectSchema(schema{"report_id": reportIDSchema(),
		"cursor": stringSchema(4096), "limit": integerSchema(1, maxMCPReportDatePage)}, "report_id")
	// The daemon owns the candidate projection. The MCP response cap still bounds
	// the whole page, including candidate quotes and source locators.
	member := objectSchema(schema{
		"document": reportIdentitySchema(), "candidates_complete": booleanSchema(),
		"candidates": arraySchema(schema{"type": "object"}, 0), //nolint:goconst // JSON Schema vocabulary is intentionally repeated.
		"selection":  schema{"type": "object"}, "choice": schema{"type": "object"},
	}, "document", "candidates_complete", "candidates", "selection")
	output := rootObjectSchema(withPrivateCache(schema{
		"members": arraySchema(member, maxMCPReportDatePage), "next_cursor": stringSchema(4096),
	}), cacheRequired("members")...)
	return input, output
}

func reportControlToolHandler(lease *daemonLease, name string, validator *jsonschema.Resolved,
	logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		var output any
		var err error
		switch name {
		case "get_report_summary":
			output, err = getReportSummary(ctx, lease, request.Params.Arguments)
		case "get_report_dates":
			output, err = getReportDates(ctx, lease, request.Params.Arguments)
		case "list_report_history":
			output, err = listReportHistory(ctx, lease, request.Params.Arguments)
		case "create_report":
			output, err = createReport(ctx, lease, request.Params.Arguments)
		case "revise_report":
			output, err = reviseReport(ctx, lease, request.Params.Arguments)
		default:
			err = errors.New("unknown report control tool")
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

func listReportHistory(ctx context.Context, lease *daemonLease, raw []byte) (reportHistoryOutput, error) {
	var input struct {
		Offset int `json:"offset"`
		Limit  int `json:"limit"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return reportHistoryOutput{}, err
	}
	if input.Limit == 0 {
		input.Limit = maxMCPReportHistoryPage
	}
	if input.Offset < 0 || input.Offset > 100 || input.Limit < 1 || input.Limit > maxMCPReportHistoryPage {
		return reportHistoryOutput{}, errors.New("report history page exceeds MCP bound")
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, client *daemonconn.Connection) (store.TermReportHistoryPage, error) {
		offset, limit := int64(input.Offset), int64(input.Limit)
		result, err := client.API().ListTermReportHistory(ctx, &apiclient.ListTermReportHistoryRequestOptions{
			Query: &apiclient.ListTermReportHistoryQuery{Offset: &offset, Limit: &limit},
		})
		if err != nil {
			return store.TermReportHistoryPage{}, err
		}
		return *result, nil
	})
	if err != nil {
		return reportHistoryOutput{}, err
	}
	if len(page.Items) > input.Limit || page.Total < len(page.Items) || page.Total > 100 {
		return reportHistoryOutput{}, errors.New("report history page exceeds MCP bound")
	}
	if page.Items == nil {
		page.Items = []store.TermReportHistory{}
	}
	return reportHistoryOutput{privateCache: newPrivateCache(), TermReportHistoryPage: page}, nil
}

func getReportSummary(ctx context.Context, lease *daemonLease, raw []byte) (reportSummaryOutput, error) {
	var input struct {
		ReportID string `json:"report_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return reportSummaryOutput{}, err
	}
	summary, err := daemonRead(ctx, lease, func(ctx context.Context, client *daemonconn.Connection) (report.Summary, error) {
		return client.GetTermReport(ctx, input.ReportID)
	})
	return reportSummaryOutput{privateCache: newPrivateCache(), Summary: summary}, err
}

func getReportDates(ctx context.Context, lease *daemonLease, raw []byte) (reportDatesOutput, error) {
	var input struct {
		ReportID string `json:"report_id"`
		Cursor   string `json:"cursor"`
		Limit    int    `json:"limit"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return reportDatesOutput{}, err
	}
	if input.Limit == 0 {
		input.Limit = maxMCPReportDatePage
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, client *daemonconn.Connection) (report.DatePage, error) {
		return client.TermReportDates(ctx, input.ReportID, report.DatePageRequest{Cursor: input.Cursor, Limit: input.Limit})
	})
	if err != nil {
		return reportDatesOutput{}, err
	}
	if len(page.Members) > maxMCPReportDatePage {
		return reportDatesOutput{}, errors.New("report date page exceeds MCP bound")
	}
	if page.Members == nil {
		page.Members = []report.DateReviewMember{}
	}
	for index := range page.Members {
		if page.Members[index].Candidates == nil {
			page.Members[index].Candidates = []report.DateCandidate{}
		}
	}
	return reportDatesOutput{privateCache: newPrivateCache(), DatePage: page}, nil
}

func createReport(ctx context.Context, lease *daemonLease, raw []byte) (reportSummaryOutput, error) {
	var input struct {
		Request report.Request `json:"request"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return reportSummaryOutput{}, err
	}
	request, err := report.NormalizeRequest(input.Request)
	if err != nil {
		return reportSummaryOutput{}, err
	}
	summary, err := daemonProcessingStart(ctx, lease, func(client *daemonconn.Connection) (report.Summary, error) {
		return client.CreateTermReport(ctx, request)
	})
	return reportSummaryOutput{privateCache: newPrivateCache(), Summary: summary}, err
}

func reviseReport(ctx context.Context, lease *daemonLease, raw []byte) (reportSummaryOutput, error) {
	var input struct {
		ReportID string              `json:"report_id"`
		Choices  []report.DateChoice `json:"choices"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return reportSummaryOutput{}, err
	}
	if len(input.Choices) == 0 || len(input.Choices) > 50000 {
		return reportSummaryOutput{}, errors.New("report revision requires bounded reviewed choices")
	}
	summary, err := daemonProcessingStart(ctx, lease, func(client *daemonconn.Connection) (report.Summary, error) {
		return client.ReviseTermReport(ctx, input.ReportID, input.Choices)
	})
	return reportSummaryOutput{privateCache: newPrivateCache(), Summary: summary}, err
}
