package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
)

func registerTextCitationRoute(api huma.API, d Deps) {
	type output struct {
		CacheControl string `header:"Cache-Control"`
		Body         document.ResolvedTextCitation
	}
	huma.Register(api, huma.Operation{
		OperationID: "resolveTextCitation", Method: http.MethodPost,
		Path:    "/api/v1/text-citations/resolve",
		Summary: "Resolve an exact quotation from retained rendition text", MaxBodyBytes: 16 << 10,
		Errors: []int{400, 401, 404, 408, 413, 416, 422, 500, 503, 504},
	}, func(ctx context.Context, input *struct {
		Body    document.TextCitation
		RawBody []byte
	}) (*output, error) {
		var citation document.TextCitation
		if err := json.Unmarshal(input.RawBody, &citation, json.RejectUnknownMembers(true)); err != nil {
			return nil, NewError(http.StatusBadRequest, "invalid_text_citation", "text citation is invalid")
		}
		if err := document.ValidateTextCitation(citation); err != nil {
			return nil, textCitationProblem(err)
		}
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		result, err := d.Processing.ResolveTextCitation(ctx, citation)
		if err != nil {
			problem := textCitationProblem(err)
			if problem.Status == http.StatusInternalServerError {
				d.Logger.ErrorContext(ctx, "resolving text citation", "error_code", problem.Code, "error", err)
			}
			return nil, problem
		}
		return &output{CacheControl: "no-store", Body: result}, nil
	})
}

func textCitationProblem(err error) *Error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return NewError(504, "citation_timeout", "text citation read timed out")
	case errors.Is(err, context.Canceled):
		return NewError(408, "citation_canceled", "text citation read was canceled")
	case errors.Is(err, document.ErrInvalidTextCitation):
		return NewError(422, "invalid_text_citation", "text citation is invalid")
	case errors.Is(err, document.ErrCitationUnavailable):
		return NewError(404, "citation_unavailable", "cited text is unavailable")
	case errors.Is(err, document.ErrCitationLimit):
		return NewError(413, "citation_limit", "cited rendition exceeds the read limit")
	case errors.Is(err, document.ErrInvalidCitationRange):
		return NewError(416, "invalid_citation_range", "citation range exceeds the rendition")
	case errors.Is(err, document.ErrCitationIntegrity):
		return NewError(500, "citation_integrity", "cited rendition failed verification")
	default:
		return NewError(500, "citation_failed", "text citation read failed")
	}
}
