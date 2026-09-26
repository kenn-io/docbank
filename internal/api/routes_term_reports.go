package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/reporting"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

func termReportError(err error) *Error {
	if problem, ok := errors.AsType[*Error](err); ok {
		return problem
	}
	if limit, ok := errors.AsType[*report.ContentDateLimitError](err); ok {
		return NewError(http.StatusRequestEntityTooLarge, "report_limit", limit.Error())
	}
	if positioned, ok := errors.AsType[*query.ExpressionError](err); ok {
		problem := NewError(http.StatusUnprocessableEntity, "invalid_query", positioned.Message)
		if positioned.Offset != 0 || positioned.End != 0 {
			problem.Position = &ErrorPosition{Offset: positioned.Offset, End: positioned.End}
		}
		return problem
	}
	switch {
	case errors.Is(err, report.ErrVisibilityChanged):
		return NewError(http.StatusConflict, "visibility_changed", "Report source visibility changed; frozen counts and downloads are withheld.")
	case errors.Is(err, reporting.ErrUnavailable):
		return NewError(http.StatusGone, "report_unavailable", "This report handle is no longer available.")
	case errors.Is(err, reporting.ErrCapacity):
		return NewError(http.StatusServiceUnavailable, "report_capacity", "Report capacity is exhausted; retry later.")
	case errors.Is(err, report.ErrBudgetExhausted), errors.Is(err, report.ErrReportLimit):
		return NewError(http.StatusRequestEntityTooLarge, "report_limit", "The report exceeds a resource limit.")
	case errors.Is(err, reporting.ErrIncompleteCoverage):
		return NewError(http.StatusUnprocessableEntity, "incomplete_coverage", "This date-scoped report has incomplete search or family coverage.")
	case errors.Is(err, reporting.ErrIncompleteDateCoverage):
		return NewError(http.StatusUnprocessableEntity, "incomplete_date_coverage", "Some report dates need review before strict counts can be produced.")
	case errors.Is(err, reporting.ErrInvalidRevision), errors.Is(err, report.ErrInvalidChoice):
		return NewError(http.StatusBadRequest, "invalid_report_choice", err.Error())
	case errors.Is(err, report.ErrStaleChoice), errors.Is(err, reporting.ErrStaleRenditionEvidence),
		errors.Is(err, store.ErrReportSelectionChanged):
		return NewError(http.StatusConflict, "stale_evidence", "The reviewed or captured evidence does not match this report.")
	case errors.Is(err, reporting.ErrReviewRequired):
		return NewError(http.StatusConflict, "date_review_required", "Review the report dates before downloading counts.")
	case errors.Is(err, context.DeadlineExceeded):
		return NewError(http.StatusServiceUnavailable, "report_timeout", "The report did not finish within its time limit.")
	case errors.Is(err, store.ErrInvalidCoverageSelection):
		return NewError(http.StatusUnprocessableEntity, "invalid_profile", "Select a configured processing profile.")
	case errors.Is(err, store.ErrUnknownReportCollection):
		return NewError(http.StatusUnprocessableEntity, "invalid_report_scope", "Select an existing collection.")
	default:
		if problem, ok := errors.AsType[*Error](FromStoreError(err)); ok {
			return problem
		}
		return NewError(http.StatusInternalServerError, "internal", err.Error())
	}
}

func withholdTermReportHistory(summary *report.Summary, state string) {
	summary.State = state
	summary.Counts = nil
	summary.Coverage = report.Coverage{}
	summary.RowCoverage = nil
	summary.UnresolvedDates = 0
	summary.CSVSHA256, summary.BundleSHA256 = "", ""
	summary.CSVBytes, summary.BundleBytes = 0, 0
}

func registerTermReportRoutes(api huma.API, d Deps, gate *OperationGate, cache *reporting.Cache,
	downloads *webDownloadRegistry, sessions *webSessionRegistry,
) {
	termReportOwner := func(ctx context.Context) (string, error) {
		// Search exports aggregate and cache across the whole vault. Until the
		// report service can fence sources before counting and retrieval, only
		// the authenticated local administrator may enter any report route.
		principal, ok := PrincipalFromContext(ctx)
		if !ok || !principal.Local {
			return "", operationHTTPError(ErrOperationDenied, false)
		}
		owner, ok := workspaceSnapshotOwner(ctx)
		if !ok {
			return "", NewError(http.StatusUnauthorized, "unauthorized", "authenticated report owner is missing")
		}
		if cache == nil || d.Store == nil || d.Blobs == nil {
			return "", NewError(http.StatusServiceUnavailable, "report_unavailable", "Search exports are unavailable.")
		}
		return owner, nil
	}
	serviceFor := func(profile string) *reporting.Service {
		return &reporting.Service{Source: d.Store, Visibility: d.Store.CheckTermReportVisibility,
			Text: reporting.CapturedTextReader{Open: d.Blobs.OpenStreamContext},
			Coverage: func(context.Context) (report.CoverageSelection, error) {
				selection, err := selectCollectionProfile(d.Cfg, profile)
				if err != nil {
					return report.CoverageSelection{}, err
				}
				if selection.Coverage.Configuration == "profile_required" {
					return report.CoverageSelection{}, store.ErrInvalidCoverageSelection
				}
				return report.CoverageSelection{Configuration: selection.Coverage.Configuration,
					ProfileFingerprint: selection.Coverage.ProfileFingerprint}, nil
			},
			Capture: gate.CaptureContext}
	}
	huma.Register(api, huma.Operation{OperationID: "createTermReport", Method: http.MethodPost,
		Path: "/api/v1/search-exports", Summary: "Prepare a frozen search export",
		MaxBodyBytes: 8 << 20}, func(ctx context.Context, in *struct{ Body report.Request }) (*struct{ Body report.Summary }, error) {
		owner, err := termReportOwner(ctx)
		if err != nil {
			return nil, err
		}
		request, err := report.NormalizeRequest(in.Body)
		if err != nil {
			if errors.Is(err, report.ErrInvalidChoice) {
				return nil, termReportError(err)
			}
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_report_request", err.Error())
		}
		summary, err := cache.Create(ctx, owner, serviceFor(request.Profile), request)
		if err != nil {
			return nil, termReportError(err)
		}
		members, err := cache.MemberIdentities(ctx, owner, summary.ID)
		if err != nil {
			cache.Drop(owner, summary.ID)
			return nil, termReportError(err)
		}
		if err := gate.MutateContext(ctx, func() error {
			return d.Store.SaveTermReportHistory(ctx, store.TermReportHistory{Request: request, Summary: summary,
				VisibilityKnown: true, VisibilityMembers: members})
		}); err != nil {
			cache.Drop(owner, summary.ID)
			return nil, FromStoreError(err)
		}
		return &struct{ Body report.Summary }{Body: summary}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "listTermReportHistory", Method: http.MethodGet,
		Path: "/api/v1/search-exports", Summary: "List recent exports"},
		func(ctx context.Context, in *struct {
			Offset int `query:"offset" minimum:"0" maximum:"100"`
			Limit  int `query:"limit" minimum:"0" maximum:"50"`
		}) (*struct{ Body store.TermReportHistoryPage }, error) {
			owner, err := termReportOwner(ctx)
			if err != nil {
				return nil, err
			}
			limit := in.Limit
			if limit == 0 {
				limit = 20
			}
			page, err := d.Store.ListTermReportHistory(ctx, in.Offset, limit)
			if err != nil {
				return nil, FromStoreError(err)
			}
			for i := range page.Items {
				item := &page.Items[i]
				if item.VisibilityKnown {
					frame := report.Frame{Members: make([]report.Member, len(item.VisibilityMembers))}
					for j, identity := range item.VisibilityMembers {
						frame.Members[j].Identity = identity
					}
					if err := d.Store.CheckTermReportVisibility(ctx, frame); err != nil {
						if !errors.Is(err, report.ErrVisibilityChanged) {
							return nil, FromStoreError(err)
						}
						withholdTermReportHistory(&item.Summary, "visibility_changed")
					}
					continue
				}
				_, visibilityErr := cache.SummaryContext(ctx, owner, item.Summary.ID)
				switch {
				case visibilityErr == nil:
				case errors.Is(visibilityErr, report.ErrVisibilityChanged):
					withholdTermReportHistory(&item.Summary, "visibility_changed")
				case errors.Is(visibilityErr, reporting.ErrUnavailable):
					withholdTermReportHistory(&item.Summary, "history_unavailable")
				default:
					return nil, termReportError(visibilityErr)
				}
			}
			return &struct{ Body store.TermReportHistoryPage }{Body: page}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "getTermReport", Method: http.MethodGet,
		Path: "/api/v1/search-exports/{id}", Summary: "Read a frozen export summary"},
		func(ctx context.Context, in *struct {
			ID string `path:"id"`
		}) (*struct{ Body report.Summary }, error) {
			owner, err := termReportOwner(ctx)
			if err != nil {
				return nil, err
			}
			summary, err := cache.SummaryContext(ctx, owner, in.ID)
			if err != nil {
				return nil, termReportError(err)
			}
			return &struct{ Body report.Summary }{Body: summary}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "getTermReportDates", Method: http.MethodPost,
		Path: "/api/v1/search-exports/{id}/dates", Summary: "Inspect frozen export date evidence",
		MaxBodyBytes: 4 << 10}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body report.DatePageRequest
	}) (*struct{ Body report.DatePage }, error) {
		owner, err := termReportOwner(ctx)
		if err != nil {
			return nil, err
		}
		page, err := cache.Dates(ctx, owner, in.ID, in.Body)
		if err != nil {
			return nil, termReportError(err)
		}
		return &struct{ Body report.DatePage }{Body: page}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "reviseTermReport", Method: http.MethodPost,
		Path: "/api/v1/search-exports/{id}/revisions", Summary: "Create an export with reviewed dates",
		MaxBodyBytes: 8 << 20}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			Choices []report.DateChoice `json:"choices"`
		}
	}) (*struct{ Body report.Summary }, error) {
		owner, err := termReportOwner(ctx)
		if err != nil {
			return nil, err
		}
		request, err := cache.RequestContext(ctx, owner, in.ID)
		if err != nil {
			return nil, termReportError(err)
		}
		request.DateChoices = in.Body.Choices
		if _, err := report.NormalizeRequest(request); err != nil {
			return nil, NewError(http.StatusBadRequest, "invalid_report_choice", err.Error())
		}
		// The cache reuses the parent frame; configuration is already frozen.
		summary, err := cache.Revise(ctx, owner, in.ID, serviceFor(""), in.Body.Choices)
		if err != nil {
			return nil, termReportError(err)
		}
		members, err := cache.MemberIdentities(ctx, owner, summary.ID)
		if err != nil {
			cache.Drop(owner, summary.ID)
			return nil, termReportError(err)
		}
		request.DateChoices = nil
		if err := gate.MutateContext(ctx, func() error {
			return d.Store.SaveTermReportHistory(ctx, store.TermReportHistory{Request: request, Summary: summary,
				VisibilityKnown: true, VisibilityMembers: members})
		}); err != nil {
			cache.Drop(owner, summary.ID)
			return nil, FromStoreError(err)
		}
		return &struct{ Body report.Summary }{Body: summary}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "issueTermReportDownload", Method: http.MethodPost,
		Path: "/api/v1/search-exports/{id}/download", Summary: "Issue a one-use browser export download",
		MaxBodyBytes: 1024}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			Format string `json:"format" enum:"csv,bundle"`
		}
	}) (*struct {
		Body struct {
			URL string `json:"url"`
		}
	}, error) {
		owner, err := termReportOwner(ctx)
		if err != nil {
			return nil, err
		}
		if !browserSessionRequest(ctx) {
			return nil, NewError(http.StatusForbidden, "browser_session_required", "Use an active browser session for a download ticket.")
		}
		if in.Body.Format != "csv" && in.Body.Format != "bundle" {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_format", "Select CSV or bundle.")
		}
		reader, _, _, err := cache.Acquire(ctx, owner, in.ID, in.Body.Format)
		if err != nil {
			return nil, termReportError(err)
		}
		_ = reader.Close()
		ticket := webDownloadTicket{owner: owner, reportID: in.ID, reportFormat: in.Body.Format,
			name: "search-export.csv", mediaType: "text/csv; charset=utf-8"}
		if in.Body.Format == "bundle" {
			ticket.name, ticket.mediaType = "search-export.zip", "application/zip"
		}
		var token string
		active, err := sessions.withActiveOwner(owner, func() error {
			var issueErr error
			token, issueErr = downloads.issue(ticket)
			return issueErr
		})
		if err != nil || !active {
			return nil, NewError(http.StatusGone, "report_unavailable", "This report session is no longer active.")
		}
		out := &struct {
			Body struct {
				URL string `json:"url"`
			}
		}{}
		out.Body.URL = webDownloadFilePath + "?ticket=" + token
		return out, nil
	})
	for _, format := range []string{"csv", "bundle"} {
		mediaType, filename := "text/csv; charset=utf-8", "search-export.csv"
		openAPIType := "text/csv"
		if format == "bundle" {
			mediaType, filename = "application/zip", "search-export.zip"
			openAPIType = "application/zip"
		}
		huma.Register(api, huma.Operation{OperationID: "downloadTermReport" + format, Method: http.MethodGet,
			Path: "/api/v1/search-exports/{id}/" + format, Summary: "Download frozen export " + format,
			Responses: map[string]*huma.Response{"200": {Description: "Frozen export artifact",
				Content: map[string]*huma.MediaType{openAPIType: {
					Schema: &huma.Schema{Type: openAPIStringType, Format: openAPIBinaryFormat},
				}},
			}}},
			func(ctx context.Context, in *struct {
				ID string `path:"id"`
			}) (*huma.StreamResponse, error) {
				owner, err := termReportOwner(ctx)
				if err != nil {
					return nil, err
				}
				if browserSessionRequest(ctx) {
					return nil, NewError(http.StatusForbidden, "report_ticket_required", "Browser downloads require a one-use ticket.")
				}
				reader, size, digest, err := cache.Acquire(ctx, owner, in.ID, format)
				if err != nil {
					return nil, termReportError(err)
				}
				return &huma.StreamResponse{Body: func(hctx huma.Context) {
					defer func() { _ = reader.Close() }()
					hctx.SetHeader("Content-Type", mediaType)
					hctx.SetHeader("Content-Disposition", `attachment; filename="`+filename+`"`)
					hctx.SetHeader("Content-Length", strconv.FormatInt(size, 10))
					hctx.SetHeader("Cache-Control", "no-store")
					hctx.SetHeader("X-Content-Type-Options", "nosniff")
					hctx.SetHeader("X-Docbank-Report-SHA256", digest)
					_, _ = io.Copy(hctx.BodyWriter(), reader)
				}}, nil
			})
	}
}
