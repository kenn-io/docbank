package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
)

type PassageCreateRequest struct {
	NodeID           int64  `json:"node_id" minimum:"1"`
	ContentVersionID string `json:"content_version_id"`
	RenditionBuildID string `json:"rendition_build_id"`
	AttachmentID     string `json:"attachment_id"`
	ByteStart        int    `json:"byte_start" minimum:"0"`
	ByteEnd          int    `json:"byte_end" minimum:"1"`
}

type PassageCreation struct {
	Ref       document.PassageRefV1 `json:"ref"`
	PassageID string                `json:"passage_id" pattern:"^[0-9a-f]{64}$"`
	Text      string                `json:"text"`
}

// registerPassageCreateRoute registers the owner-facing exact creation route.
func registerPassageCreateRoute(api huma.API, d Deps, g *gate) {
	type output struct{ Body PassageCreation }
	huma.Register(api, huma.Operation{OperationID: "createPassage", Method: http.MethodPost,
		Path: "/api/v1/passages/create", Summary: "Create an exact retained Markdown passage reference",
		MaxBodyBytes: 16 << 10}, func(ctx context.Context, input *struct {
		Body PassageCreateRequest
	}) (*output, error) {
		if d.Processing == nil || g == nil {
			return nil, processingUnavailable()
		}
		var created processing.PassageCreation
		err := g.mutate(func() error {
			var createErr error
			created, createErr = d.Processing.CreatePassage(ctx, processing.PassageCreateRequest{
				NodeID: input.Body.NodeID, ContentVersionID: input.Body.ContentVersionID,
				RenditionBuildID: input.Body.RenditionBuildID, AttachmentID: input.Body.AttachmentID,
				ByteStart: input.Body.ByteStart, ByteEnd: input.Body.ByteEnd,
			})
			return createErr
		})
		if err != nil {
			if _, ok := errors.AsType[*Error](err); ok {
				return nil, err
			}
			return nil, passageResolveError(err)
		}
		return &output{Body: PassageCreation{Ref: created.Ref,
			PassageID: created.PassageID, Text: created.Text}}, nil
	})
}
