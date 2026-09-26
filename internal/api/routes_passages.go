package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
)

type PassageResolveRequest struct {
	Ref      document.PassageRefV1 `json:"ref"`
	MaxBytes int                   `json:"max_bytes,omitzero" minimum:"1" maximum:"262144" default:"32768"`
}

type PassageResolution struct {
	Availability  string                      `json:"availability" enum:"available"`
	Freshness     string                      `json:"freshness" enum:"current,historical"`
	PassageID     string                      `json:"passage_id" pattern:"^[0-9a-f]{64}$"`
	Ref           document.PassageRefV1       `json:"ref"`
	Text          string                      `json:"text"`
	SectionPath   []string                    `json:"section_path"`
	SourceLocator *document.EvidenceLocatorV1 `json:"source_locator,omitempty"`
	SourcePath    string                      `json:"source_path"`
}

func registerPassageRoutes(api huma.API, d Deps) {
	type output struct{ Body PassageResolution }
	huma.Register(api, huma.Operation{OperationID: "resolvePassage", Method: http.MethodPost,
		Path: "/api/v1/passages/resolve", Summary: "Resolve one exact retained Markdown passage",
		MaxBodyBytes: 16 << 10}, func(ctx context.Context, input *struct {
		Body PassageResolveRequest
	}) (*output, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		resolved, err := d.Processing.ResolvePassage(ctx, processing.PassageResolveRequest{
			Ref: input.Body.Ref, MaxBytes: input.Body.MaxBytes,
		})
		if err != nil {
			return nil, passageResolveError(err)
		}
		return &output{Body: passageResolutionFromProcessing(resolved)}, nil
	})
}

func passageResolutionFromProcessing(value processing.PassageResolution) PassageResolution {
	var locator *document.EvidenceLocatorV1
	if value.SourceLocator != nil {
		copied := *value.SourceLocator
		locator = &copied
	}
	return PassageResolution{Availability: value.Availability, Freshness: value.Freshness,
		PassageID: value.PassageID, Ref: value.Ref, Text: value.Text,
		SectionPath:   append([]string(nil), value.SectionPath...),
		SourceLocator: locator, SourcePath: value.SourcePath}
}

func passageResolveError(err error) error {
	switch {
	case errors.Is(err, processing.ErrPassageInvalid):
		return NewError(http.StatusUnprocessableEntity, "invalid_passage",
			"The passage reference or read budget is invalid.")
	case errors.Is(err, processing.ErrPassageUnauthorized),
		errors.Is(err, processing.ErrPassageUnavailable):
		// Deliberately merge authorization and retention failures so the route
		// does not disclose whether a hidden source exists.
		return NewError(http.StatusNotFound, "passage_unavailable",
			"The exact authorized passage is not available.")
	case errors.Is(err, processing.ErrPassageCorrupt):
		return NewError(http.StatusInternalServerError, "passage_corrupt",
			"The retained passage failed identity verification.")
	default:
		return NewError(http.StatusInternalServerError, "internal",
			"The passage could not be resolved.")
	}
}
