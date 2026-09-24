package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/store"
)

type ProductionSetCreated struct {
	Set   redaction.Set   `json:"set"`
	Draft redaction.Draft `json:"draft"`
}

// ProductionMember gives this wire shape a distinct OpenAPI component name.
type ProductionMember redaction.Member

// ProductionDecision gives this wire shape a distinct OpenAPI component name.
type ProductionDecision redaction.Decision

// ProductionReceipt gives production mutation receipts a distinct wire name.
type ProductionReceipt redaction.Receipt

type ProductionChange struct {
	Kind                string              `json:"kind"`
	MemberID            string              `json:"member_id,omitzero"`
	Member              *ProductionMember   `json:"member,omitzero"`
	Decision            *ProductionDecision `json:"decision,omitzero"`
	DecisionID          string              `json:"decision_id,omitzero"`
	Mode                string              `json:"mode,omitzero"`
	RecipeID            string              `json:"recipe_id,omitzero"`
	ProfileID           string              `json:"profile_id,omitzero"`
	DisclosureProfileID string              `json:"disclosure_profile_id,omitzero"`
	NumberingRecipeID   string              `json:"numbering_recipe_id,omitzero"`
	PolicyID            string              `json:"policy_id,omitzero"`
	PolicyVersion       int64               `json:"policy_version,omitzero"`
}

func (change ProductionChange) domain() redaction.Change {
	value := redaction.Change{Kind: change.Kind, MemberID: change.MemberID,
		DecisionID: change.DecisionID, Mode: change.Mode, RecipeID: change.RecipeID,
		ProfileID: change.ProfileID, DisclosureProfileID: change.DisclosureProfileID,
		NumberingRecipeID: change.NumberingRecipeID, PolicyID: change.PolicyID,
		PolicyVersion: change.PolicyVersion}
	if change.Member != nil {
		member := redaction.Member(*change.Member)
		value.Member = &member
	}
	if change.Decision != nil {
		decision := redaction.Decision(*change.Decision)
		value.Decision = &decision
	}
	return value
}

type ProductionMemberPage struct {
	Items      []ProductionMember `json:"items"`
	NextCursor string             `json:"next_cursor"`
}

type ProductionDecisionPage struct {
	Items      []ProductionDecision `json:"items"`
	NextCursor string               `json:"next_cursor"`
}

type ProductionInstructionsRequest struct {
	OperationID  string `json:"operation_id"`
	Instructions string `json:"instructions"`
}

func (request ProductionInstructionsRequest) Domain(etag int64) redaction.InstructionsEditRequest {
	return redaction.InstructionsEditRequest{OperationID: request.OperationID, ETag: etag,
		Instructions: request.Instructions}
}

type ProductionChangesRequest struct {
	OperationID string             `json:"operation_id"`
	Changes     []ProductionChange `json:"changes"`
}

func (request ProductionChangesRequest) Domain(etag int64) redaction.ApplyRequest {
	changes := make([]redaction.Change, len(request.Changes))
	for i, change := range request.Changes {
		changes[i] = change.domain()
	}
	return redaction.ApplyRequest{OperationID: request.OperationID, ETag: etag, Changes: changes}
}

func productionSetError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return FromStoreError(err)
	case errors.Is(err, store.ErrProductionOperationConflict):
		return NewError(http.StatusConflict, "production_operation_conflict", "operation ID names different production input")
	case errors.Is(err, store.ErrProductionRevisionConflict):
		return NewError(http.StatusConflict, "production_revision_conflict", "production revision changed")
	case errors.Is(err, store.ErrInvalidProduction):
		return NewError(http.StatusUnprocessableEntity, "invalid_production", "production input is invalid")
	default:
		return NewError(http.StatusInternalServerError, "production_failed", "production operation failed")
	}
}

func registerProductionRoutes(api huma.API, d Deps, g *OperationGate) {
	huma.Register(api, huma.Operation{OperationID: "createProductionSet", Method: http.MethodPost,
		Path: "/api/v1/productions/sets", Summary: "Create an idempotent production set and first draft",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: redaction.MaxCommandBytes},
		func(ctx context.Context, in *struct{ Body redaction.CreateRequest }) (*struct{ Body ProductionSetCreated }, error) {
			actor, ok := workspaceSnapshotOwner(ctx)
			if !ok {
				return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
			}
			var set redaction.Set
			var draft redaction.Draft
			err := g.mutate(func() error {
				var err error
				set, draft, err = d.Store.CreateProductionSet(ctx, actor, in.Body)
				return err
			})
			if err != nil {
				return nil, productionSetError(err)
			}
			return &struct{ Body ProductionSetCreated }{Body: ProductionSetCreated{Set: set, Draft: draft}}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "getProductionSet", Method: http.MethodGet,
		Path: "/api/v1/productions/sets/{set_id}", Summary: "Read a production set"},
		func(ctx context.Context, in *struct {
			SetID string `path:"set_id" format:"uuid"`
		}) (*struct{ Body redaction.Set }, error) {
			set, err := d.Store.ProductionSet(ctx, in.SetID)
			if err != nil {
				return nil, productionSetError(err)
			}
			return &struct{ Body redaction.Set }{Body: set}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "getProductionDraft", Method: http.MethodGet,
		Path: "/api/v1/productions/sets/{set_id}/revisions/{revision}", Summary: "Read an exact production draft"},
		func(ctx context.Context, in *struct {
			SetID    string `path:"set_id" format:"uuid"`
			Revision int64  `path:"revision" minimum:"1"`
		}) (*struct{ Body redaction.Draft }, error) {
			draft, err := d.Store.ProductionDraft(ctx, in.SetID, in.Revision)
			if err != nil {
				return nil, productionSetError(err)
			}
			return &struct{ Body redaction.Draft }{Body: draft}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "listProductionMembers", Method: http.MethodGet,
		Path: "/api/v1/productions/sets/{set_id}/revisions/{revision}/members", Summary: "Page exact production members"},
		func(ctx context.Context, in *struct {
			SetID    string `path:"set_id" format:"uuid"`
			Revision int64  `path:"revision" minimum:"1"`
			Cursor   string `query:"cursor"`
			Limit    int    `query:"limit" minimum:"0" maximum:"200"`
		}) (*struct{ Body ProductionMemberPage }, error) {
			limit := in.Limit
			if limit == 0 {
				limit = 100
			}
			items, next, err := d.Store.ProductionMembers(ctx, in.SetID, in.Revision, in.Cursor, limit)
			if err != nil {
				return nil, productionSetError(err)
			}
			out := make([]ProductionMember, len(items))
			for i, item := range items {
				out[i] = ProductionMember(item)
			}
			return &struct{ Body ProductionMemberPage }{Body: ProductionMemberPage{Items: out, NextCursor: next}}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "listProductionDecisions", Method: http.MethodGet,
		Path: "/api/v1/productions/sets/{set_id}/revisions/{revision}/decisions", Summary: "Page exact production decisions"},
		func(ctx context.Context, in *struct {
			SetID    string `path:"set_id" format:"uuid"`
			Revision int64  `path:"revision" minimum:"1"`
			Cursor   string `query:"cursor"`
			Limit    int    `query:"limit" minimum:"0" maximum:"200"`
		}) (*struct{ Body ProductionDecisionPage }, error) {
			limit := in.Limit
			if limit == 0 {
				limit = 100
			}
			items, next, err := d.Store.ProductionDecisions(ctx, in.SetID, in.Revision, in.Cursor, limit)
			if err != nil {
				return nil, productionSetError(err)
			}
			out := make([]ProductionDecision, len(items))
			for i, item := range items {
				out[i] = ProductionDecision(item)
			}
			return &struct{ Body ProductionDecisionPage }{Body: ProductionDecisionPage{Items: out, NextCursor: next}}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "editProductionInstructions", Method: http.MethodPut,
		Path:         "/api/v1/productions/sets/{set_id}/revisions/{revision}/instructions",
		Summary:      "Edit production instructions with an exact draft ETag and replay-safe operation ID",
		MaxBodyBytes: redaction.MaxCommandBytes},
		func(ctx context.Context, in *struct {
			SetID    string `path:"set_id" format:"uuid"`
			Revision int64  `path:"revision" minimum:"1"`
			IfMatch  string `header:"If-Match"`
			Body     ProductionInstructionsRequest
		}) (*struct{ Body ProductionReceipt }, error) {
			etag, err := parseIfMatch(in.IfMatch)
			if err != nil {
				return nil, err
			}
			actor, ok := workspaceSnapshotOwner(ctx)
			if !ok {
				return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
			}
			request := in.Body.Domain(etag)
			var receipt redaction.Receipt
			err = g.mutate(func() error {
				var err error
				receipt, err = d.Store.EditProductionInstructions(ctx, actor, in.SetID, in.Revision, request)
				return err
			})
			if err != nil {
				return nil, productionSetError(err)
			}
			return &struct{ Body ProductionReceipt }{Body: ProductionReceipt(receipt)}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "applyProductionChanges", Method: http.MethodPost,
		Path:         "/api/v1/productions/sets/{set_id}/revisions/{revision}/changes",
		Summary:      "Apply a bounded production change batch with an exact draft ETag",
		MaxBodyBytes: redaction.MaxCommandBytes},
		func(ctx context.Context, in *struct {
			SetID    string `path:"set_id" format:"uuid"`
			Revision int64  `path:"revision" minimum:"1"`
			IfMatch  string `header:"If-Match"`
			Body     ProductionChangesRequest
		}) (*struct{ Body ProductionReceipt }, error) {
			etag, err := parseIfMatch(in.IfMatch)
			if err != nil {
				return nil, err
			}
			actor, ok := workspaceSnapshotOwner(ctx)
			if !ok {
				return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
			}
			request := in.Body.Domain(etag)
			var receipt redaction.Receipt
			err = g.mutate(func() error {
				var err error
				receipt, err = d.Store.ApplyProductionChanges(ctx, actor, in.SetID, in.Revision, request)
				return err
			})
			if err != nil {
				return nil, productionSetError(err)
			}
			return &struct{ Body ProductionReceipt }{Body: ProductionReceipt(receipt)}, nil
		})
}
