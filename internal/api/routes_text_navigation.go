package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
)

func registerTextNavigationRoutes(api huma.API, d Deps) {
	registerTextOutlineRoute(api, d)
	registerTextSectionRoute(api, d)
}

func registerTextOutlineRoute(api huma.API, d Deps) {
	type output struct {
		CacheControl string `header:"Cache-Control"`
		Body         document.TextOutlinePage
	}
	huma.Register(api, huma.Operation{
		OperationID: "getTextOutline", Method: http.MethodPost, Path: "/api/v1/text-citations/outline",
		Summary: "Read the outline of an exact retained rendition", MaxBodyBytes: 16 << 10,
		Errors: []int{400, 401, 404, 408, 413, 416, 422, 500, 503, 504},
	}, func(ctx context.Context, input *struct {
		Body    document.TextOutlineRequest
		RawBody []byte
	}) (*output, error) {
		var request document.TextOutlineRequest
		if err := json.Unmarshal(input.RawBody, &request, json.RejectUnknownMembers(true)); err != nil {
			return nil, NewError(400, "invalid_text_navigation", "text navigation is invalid")
		}
		if err := document.ValidateTextOutlineRequest(request); err != nil {
			return nil, textNavigationProblem(err)
		}
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		result, err := d.Processing.GetTextOutline(ctx, request)
		if err != nil {
			problem := textNavigationProblem(err)
			if problem.Status == 500 {
				d.Logger.ErrorContext(ctx, "reading retained text outline",
					"error_code", problem.Code, "error", err)
			}
			return nil, problem
		}
		return &output{CacheControl: "no-store", Body: result}, nil
	})
}

func registerTextSectionRoute(api huma.API, d Deps) {
	type output struct {
		CacheControl string `header:"Cache-Control"`
		Body         document.TextSectionPage
	}
	huma.Register(api, huma.Operation{
		OperationID: "readTextSection", Method: http.MethodPost, Path: "/api/v1/text-citations/section",
		Summary: "Read a page of an exact retained text section", MaxBodyBytes: 16 << 10,
		Errors: []int{400, 401, 404, 408, 413, 416, 422, 500, 503, 504},
	}, func(ctx context.Context, input *struct {
		Body    document.TextSectionRequest
		RawBody []byte
	}) (*output, error) {
		var request document.TextSectionRequest
		if err := json.Unmarshal(input.RawBody, &request, json.RejectUnknownMembers(true)); err != nil {
			return nil, NewError(400, "invalid_text_navigation", "text navigation is invalid")
		}
		if err := document.ValidateTextSectionRequest(request); err != nil {
			return nil, textNavigationProblem(err)
		}
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		result, err := d.Processing.ReadTextSection(ctx, request)
		if err != nil {
			problem := textNavigationProblem(err)
			if problem.Status == 500 {
				d.Logger.ErrorContext(ctx, "reading retained text section",
					"error_code", problem.Code, "error", err)
			}
			return nil, problem
		}
		return &output{CacheControl: "no-store", Body: result}, nil
	})
}

func textNavigationProblem(err error) *Error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return textCitationProblem(err)
	case errors.Is(err, document.ErrInvalidTextNavigation):
		return NewError(422, "invalid_text_navigation", "section bounds or navigation offset are invalid")
	case errors.Is(err, document.ErrTextNavigationLimit):
		return NewError(422, "text_navigation_limit", "retained text exceeds the navigation limit")
	default:
		return textCitationProblem(err)
	}
}
