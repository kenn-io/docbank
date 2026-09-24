package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

type ContextPackRequest struct {
	Fence                 DocumentSourceFence    `json:"fence"`
	Query                 string                 `json:"query,omitzero" maxLength:"8192"`
	Seed                  *document.PassageRefV1 `json:"seed,omitempty"`
	Profile               string                 `json:"profile,omitzero" maxLength:"128"`
	MaxBytes              int                    `json:"max_bytes,omitzero" minimum:"1" maximum:"262144" default:"65536"`
	PerDocumentPassages   int                    `json:"per_document_passages,omitzero" minimum:"1" maximum:"8" default:"2"`
	MaxDocuments          int                    `json:"max_documents,omitzero" minimum:"1" maximum:"20" default:"20"`
	IncludeSectionContext bool                   `json:"include_section_context,omitzero"`
}

type ContextPackResponse processing.ContextPack

func registerContextPackRoutes(api huma.API, d Deps) {
	type output struct{ Body ContextPackResponse }
	huma.Register(api, huma.Operation{OperationID: "createContextPack", Method: http.MethodPost,
		Path: "/api/v1/context-packs", Summary: "Read bounded exact context from a source fence",
		MaxBodyBytes: 512 << 10}, func(ctx context.Context, input *struct {
		Body ContextPackRequest
	}) (*output, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		request := input.Body
		pack, err := d.Processing.ContextPack(ctx, processing.ContextPackRequest{
			Fence: processing.SourceFence{VaultUID: request.Fence.VaultUID,
				ContentVersionIDs: request.Fence.ContentVersionIDs},
			Query: request.Query, Seed: request.Seed, Profile: request.Profile,
			MaxBytes: request.MaxBytes, PerDocumentPassages: request.PerDocumentPassages,
			MaxDocuments: request.MaxDocuments, IncludeSectionContext: request.IncludeSectionContext,
		})
		if err != nil {
			switch {
			case errors.Is(err, processing.ErrContextInvalid):
				return nil, NewError(http.StatusUnprocessableEntity, "invalid_context_pack", "The context pack request is invalid.")
			case errors.Is(err, processing.ErrContextBudget):
				return nil, NewError(http.StatusUnprocessableEntity, "context_budget_too_small", "The byte budget cannot hold context metadata.")
			case errors.Is(err, processing.ErrPassageUnavailable), errors.Is(err, processing.ErrPassageUnauthorized):
				return nil, NewError(http.StatusNotFound, "passage_unavailable", "The exact authorized passage is not available.")
			case errors.Is(err, processing.ErrPassageCorrupt):
				return nil, passageResolveError(err)
			case errors.Is(err, store.ErrLexicalGenerationStale):
				return nil, NewError(http.StatusConflict, "index_changed", "The lexical index changed during context assembly.")
			default:
				return nil, fromProcessingError(err)
			}
		}
		return &output{Body: ContextPackResponse(pack)}, nil
	})
}
