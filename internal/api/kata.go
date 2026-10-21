package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/kataclient"
	"go.kenn.io/docbank/internal/kataevidence"
	"go.kenn.io/docbank/internal/store"
)

func kataService(d Deps, g *OperationGate) (kataevidence.Service, error) {
	cfg, err := d.Cfg.Integrations.Kata.Resolve(d.VaultRoot)
	if err != nil {
		return kataevidence.Service{}, NewError(503, "kata_unconfigured", err.Error()+"; restart the daemon after changing [integrations.kata]")
	}
	return kataevidence.Service{Kata: kataclient.New(cfg), Archive: kataArchive{deps: d, gate: g}}, nil
}

func registerKataRoutes(api huma.API, d Deps, g *OperationGate) {
	type citeOutput struct {
		CacheControl string `header:"Cache-Control"`
		Body         kataclient.Issue
	}
	huma.Register(api, huma.Operation{OperationID: "citeKataEvidence", Method: http.MethodPost, Path: "/api/v1/kata/cite",
		Summary: "Quote a verified document passage into a new or existing Kata issue", MaxBodyBytes: 32 << 10,
		Errors: []int{400, 401, 403, 404, 409, 413, 422, 502, 503, 504}}, func(ctx context.Context, in *struct {
		Body    kataevidence.CiteRequest
		RawBody []byte
	}) (*citeOutput, error) {
		var request kataevidence.CiteRequest
		if json.Unmarshal(in.RawBody, &request, json.RejectUnknownMembers(true)) != nil {
			return nil, kataProblem(kataevidence.ErrInvalid)
		}
		if err := clearRequestReadDeadline(ctx); err != nil {
			return nil, NewError(
				http.StatusInternalServerError,
				"kata_request_setup_failed",
				"Kata evidence request could not be prepared",
			)
		}
		service, err := kataService(d, g)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		issue, err := service.Cite(ctx, request)
		if err != nil {
			return nil, kataProblem(err)
		}
		return &citeOutput{CacheControl: "no-store", Body: issue}, nil
	})
	type contextOutput struct {
		CacheControl string `header:"Cache-Control"`
		Body         kataevidence.Context
	}
	huma.Register(api, huma.Operation{OperationID: "getKataContext", Method: http.MethodPost, Path: "/api/v1/kata/context",
		Summary: "Read Docbank evidence and its current state from a Kata issue", MaxBodyBytes: 4096,
		Errors: []int{400, 401, 403, 404, 409, 413, 422, 502, 503, 504}}, func(ctx context.Context, in *struct {
		Body    kataevidence.ContextRequest
		RawBody []byte
	}) (*contextOutput, error) {
		var request kataevidence.ContextRequest
		if json.Unmarshal(in.RawBody, &request, json.RejectUnknownMembers(true)) != nil {
			return nil, kataProblem(kataevidence.ErrInvalid)
		}
		if err := clearRequestReadDeadline(ctx); err != nil {
			return nil, NewError(
				http.StatusInternalServerError,
				"kata_request_setup_failed",
				"Kata evidence request could not be prepared",
			)
		}
		service, err := kataService(d, g)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		page, err := service.Context(ctx, request)
		if err != nil {
			return nil, kataProblem(err)
		}
		return &contextOutput{CacheControl: "no-store", Body: page}, nil
	})
	// jsontext.Value carries arbitrary JSON, rather than base64-encoded bytes.
	api.OpenAPI().Components.Schemas.Map()["Issue"].Properties["metadata"].AdditionalProperties = &huma.Schema{}
}

func kataProblem(err error) *Error {
	if failure, ok := errors.AsType[*kataclient.Error](err); ok {
		switch failure.Status {
		case http.StatusUnauthorized:
			return NewError(401, "kata_unauthorized", "Kata rejected the configured credential; update the Kata connection")
		case http.StatusForbidden:
			return NewError(403, "kata_forbidden", "Kata denied permission for this operation")
		case http.StatusNotFound:
			return NewError(404, "kata_issue_unavailable", "Kata issue or project is unavailable")
		}
		switch failure.Code {
		case "incompatible_api":
			return NewError(503, "kata_incompatible", "Kata API schema 0.21.0 or newer is required")
		case "invalid_reference", "wrong_project", "project_unavailable":
			return NewError(422, "invalid_kata_evidence", "Kata issue reference is invalid, unavailable, or outside the configured project")
		}
	}
	switch {
	case errors.Is(err, document.ErrInvalidTextCitation):
		return NewError(422, "invalid_kata_evidence", "--citation is invalid")
	case errors.Is(err, kataevidence.ErrIdempotencyConflict):
		return NewError(409, "kata_idempotency_conflict", err.Error())
	case errors.Is(err, document.ErrInvalidCitationRange):
		return NewError(422, "invalid_kata_evidence", "--end or --start is outside the selected text")
	case errors.Is(err, kataevidence.ErrInvalid):
		return NewError(422, "invalid_kata_evidence", err.Error())
	case errors.Is(err, kataevidence.ErrChanged), errors.Is(err, kataevidence.ErrConflict):
		return NewError(409, "kata_evidence_changed", err.Error())
	case errors.Is(err, kataevidence.ErrUnavailable), errors.Is(err, store.ErrNotFound), errors.Is(err, document.ErrCitationUnavailable):
		return NewError(404, "kata_evidence_unavailable", "selected document passage is unavailable")
	case errors.Is(err, kataevidence.ErrUnsupported):
		return NewError(409, "kata_evidence_unsupported", err.Error())
	case errors.Is(err, kataevidence.ErrUnsupportedSource):
		return NewError(409, "kata_evidence_unsupported", err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return NewError(504, "kata_timeout", "Kata evidence operation timed out; retry creates with the same key")
	default:
		return NewError(502, "kata_request_failed", "Kata evidence operation failed; retry creates with the same key and appends with the same selection")
	}
}
