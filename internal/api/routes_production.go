package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

// ProductionRecipe has a distinct OpenAPI name for the qualified renderer contract.
type ProductionRecipe redaction.Recipe

type ProductionRecipeOption struct {
	ID     string           `json:"id"`
	SHA256 string           `json:"sha256"`
	Recipe ProductionRecipe `json:"recipe"`
}

type ProductionRecipeCatalog struct {
	DefaultID string                   `json:"default_id"`
	Items     []ProductionRecipeOption `json:"items"`
}

// QualifiedProductionRecipes reads the two renderer recipes built into this release.
func QualifiedProductionRecipes() (ProductionRecipeCatalog, error) {
	result := ProductionRecipeCatalog{DefaultID: redaction.DefaultRecipeID,
		Items: make([]ProductionRecipeOption, 0, 2)}
	for _, choice := range []struct {
		id  string
		dpi int
	}{{redaction.RecipeID300DPI, 300}, {redaction.RecipeID600DPI, 600}} {
		recipe, err := pdfproduction.QualifiedRecipeForDPI(choice.dpi)
		if err != nil {
			return ProductionRecipeCatalog{}, err
		}
		encoded, err := canonical.Marshal(recipe)
		if err != nil {
			return ProductionRecipeCatalog{}, err
		}
		digest := sha256.Sum256(encoded)
		result.Items = append(result.Items, ProductionRecipeOption{ID: choice.id,
			SHA256: hex.EncodeToString(digest[:]), Recipe: ProductionRecipe(recipe)})
	}
	return result, nil
}

type ProductionSetCreated struct {
	Set   redaction.Set   `json:"set"`
	Draft redaction.Draft `json:"draft"`
}

type ProductionSetPage struct {
	Items      []redaction.Set `json:"items"`
	NextCursor string          `json:"next_cursor"`
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

// ProductionMapChunk carries one digest-bound page of canonical aligned-map
// JSON. Its base64 data is binary-safe because pages may split UTF-8 bytes.
type ProductionMapChunk struct {
	MapSHA256   string `json:"map_sha256"`
	Offset      int64  `json:"offset"`
	TotalBytes  int64  `json:"total_bytes"`
	Data        string `json:"data"`
	ChunkSHA256 string `json:"chunk_sha256"`
	NextCursor  string `json:"next_cursor"`
}

// ProductionJobStatus is the public, bounded state projection of a retained job.
type ProductionJobStatus store.ProductionJobStatus

type ProductionJobAdmissionRequest struct {
	JobID       string `json:"job_id"`
	OperationID string `json:"operation_id"`
}

type ProductionJobCancelRequest struct {
	OperationID string `json:"operation_id"`
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

type ProductionMembershipSealRequest struct {
	OperationID string `json:"operation_id"`
	Total       int    `json:"total"`
	MemberHash  string `json:"member_hash"`
}

func (request ProductionMembershipSealRequest) Domain(etag int64) redaction.MembershipSealRequest {
	return redaction.MembershipSealRequest{OperationID: request.OperationID, ETag: etag,
		Total: request.Total, MemberHash: request.MemberHash}
}

type ProductionMemberReviewRequest struct {
	OperationID string `json:"operation_id"`
	Binding     string `json:"binding"`
	Complete    bool   `json:"complete"`
}

type ProductionForkRequest struct {
	OperationID string `json:"operation_id"`
}

func (request ProductionMemberReviewRequest) Domain(etag int64, memberID string) store.ProductionReviewRequest {
	return store.ProductionReviewRequest{OperationID: request.OperationID, ETag: etag,
		MemberID: memberID, Binding: request.Binding, Complete: request.Complete}
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
	case errors.Is(err, productionservice.ErrJobConflict):
		return NewError(http.StatusConflict, "production_job_conflict", "production job cannot be changed")
	case errors.Is(err, store.ErrInvalidProduction):
		return NewError(http.StatusUnprocessableEntity, "invalid_production", "production input is invalid")
	default:
		return NewError(http.StatusInternalServerError, "production_failed", "production operation failed")
	}
}

func registerProductionRoutes(api huma.API, d Deps, g *OperationGate) {
	huma.Register(api, huma.Operation{OperationID: "listProductionRecipes", Method: http.MethodGet,
		Path: "/api/v1/productions/recipes", Summary: "List qualified production rendering recipes"},
		func(ctx context.Context, in *struct{}) (*struct{ Body ProductionRecipeCatalog }, error) {
			catalog, err := QualifiedProductionRecipes()
			if err != nil {
				return nil, NewError(http.StatusInternalServerError, "production_catalog_unavailable",
					"qualified production recipes are unavailable")
			}
			return &struct{ Body ProductionRecipeCatalog }{Body: catalog}, nil
		})
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
	huma.Register(api, huma.Operation{OperationID: "listProductionSets", Method: http.MethodGet,
		Path: "/api/v1/productions/sets", Summary: "Page retained production sets"},
		func(ctx context.Context, in *struct {
			Cursor string `query:"cursor" maxLength:"2048"`
			Limit  int    `query:"limit" minimum:"0" maximum:"200"`
		}) (*struct{ Body ProductionSetPage }, error) {
			limit := in.Limit
			if limit == 0 {
				limit = 100
			}
			items, next, err := d.Store.ListProductionSets(ctx, in.Cursor, limit)
			if err != nil {
				return nil, productionSetError(err)
			}
			return &struct{ Body ProductionSetPage }{Body: ProductionSetPage{Items: items, NextCursor: next}}, nil
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
			SetID     string `path:"set_id" format:"uuid"`
			Revision  int64  `path:"revision" minimum:"1"`
			Cursor    string `query:"cursor"`
			Limit     int    `query:"limit" minimum:"0" maximum:"500"`
			Uncertain string `query:"uncertain" enum:"true,false"`
		}) (*struct{ Body ProductionDecisionPage }, error) {
			var uncertain *bool
			switch in.Uncertain {
			case "":
			case "true":
				uncertain = new(true)
			case "false":
				uncertain = new(false)
			default:
				return nil, NewError(http.StatusUnprocessableEntity, "invalid_production", "invalid uncertainty filter")
			}
			limit := in.Limit
			if limit == 0 {
				limit = 100
			}
			items, next, err := d.Store.ProductionDecisionsFiltered(ctx, in.SetID, in.Revision, in.Cursor, limit, uncertain)
			if err != nil {
				return nil, productionSetError(err)
			}
			out := make([]ProductionDecision, len(items))
			for i, item := range items {
				out[i] = ProductionDecision(item)
			}
			return &struct{ Body ProductionDecisionPage }{Body: ProductionDecisionPage{Items: out, NextCursor: next}}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "getProductionMapChunk", Method: http.MethodGet,
		Path:    "/api/v1/productions/sets/{set_id}/revisions/{revision}/maps/{member_id}",
		Summary: "Page the exact retained aligned-text map for one production member"},
		func(ctx context.Context, in *struct {
			SetID    string `path:"set_id" format:"uuid"`
			Revision int64  `path:"revision" minimum:"1"`
			MemberID string `path:"member_id" format:"uuid"`
			Cursor   string `query:"cursor" maxLength:"512"`
			Limit    int    `query:"limit" minimum:"0" maximum:"65536"`
		}) (*struct{ Body ProductionMapChunk }, error) {
			limit := in.Limit
			if limit == 0 {
				limit = store.MaxProductionMapChunkBytes
			}
			chunk, err := d.Store.ProductionMapChunk(ctx, in.SetID, in.Revision, in.MemberID, in.Cursor, limit)
			if err != nil {
				return nil, productionSetError(err)
			}
			return &struct{ Body ProductionMapChunk }{Body: ProductionMapChunk(chunk)}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "admitProductionJob", Method: http.MethodPost,
		Path:    "/api/v1/productions/sets/{set_id}/revisions/{revision}/jobs",
		Summary: "Admit one production job from finalized authority", DefaultStatus: http.StatusCreated,
		MaxBodyBytes: 4096},
		func(ctx context.Context, in *struct {
			SetID    string `path:"set_id" format:"uuid"`
			Revision int64  `path:"revision" minimum:"1"`
			IfMatch  string `header:"If-Match"`
			Body     ProductionJobAdmissionRequest
		}) (*struct{ Body ProductionJobStatus }, error) {
			etag, err := parseIfMatch(in.IfMatch)
			if err != nil {
				return nil, err
			}
			if _, ok := workspaceSnapshotOwner(ctx); !ok {
				return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
			}
			var status store.ProductionJobStatus
			err = g.mutate(func() error {
				if _, err := d.Store.AdmitFinalizedProductionJob(ctx, in.SetID, in.Revision, etag,
					in.Body.JobID, in.Body.OperationID); err != nil {
					return err
				}
				status, err = d.Store.ProductionJobStatus(ctx, in.SetID, in.Body.JobID)
				return err
			})
			if err != nil {
				return nil, productionSetError(err)
			}
			return &struct{ Body ProductionJobStatus }{Body: ProductionJobStatus(status)}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "getProductionJobStatus", Method: http.MethodGet,
		Path:    "/api/v1/productions/sets/{set_id}/jobs/{job_id}",
		Summary: "Read one set-scoped production job status"},
		func(ctx context.Context, in *struct {
			SetID string `path:"set_id" format:"uuid"`
			JobID string `path:"job_id" format:"uuid"`
		}) (*struct{ Body ProductionJobStatus }, error) {
			status, err := d.Store.ProductionJobStatus(ctx, in.SetID, in.JobID)
			if err != nil {
				return nil, productionSetError(err)
			}
			return &struct{ Body ProductionJobStatus }{Body: ProductionJobStatus(status)}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "cancelProductionJob", Method: http.MethodPost,
		Path:    "/api/v1/productions/sets/{set_id}/jobs/{job_id}/cancel",
		Summary: "Cancel one production job with a replay-safe operation ID", MaxBodyBytes: 4096},
		func(ctx context.Context, in *struct {
			SetID   string `path:"set_id" format:"uuid"`
			JobID   string `path:"job_id" format:"uuid"`
			IfMatch string `header:"If-Match"`
			Body    ProductionJobCancelRequest
		}) (*struct{ Body ProductionReceipt }, error) {
			etag, err := parseIfMatch(in.IfMatch)
			if err != nil {
				return nil, err
			}
			actor, ok := workspaceSnapshotOwner(ctx)
			if !ok {
				return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
			}
			var receipt redaction.Receipt
			err = g.mutate(func() error {
				var err error
				receipt, err = d.Store.CancelProductionJobOperation(ctx, actor, in.SetID, in.JobID, etag, in.Body.OperationID)
				return err
			})
			if err != nil {
				return nil, productionSetError(err)
			}
			return &struct{ Body ProductionReceipt }{Body: ProductionReceipt(receipt)}, nil
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
	huma.Register(api, huma.Operation{OperationID: "sealProductionMembership", Method: http.MethodPost,
		Path:    "/api/v1/productions/sets/{set_id}/revisions/{revision}/seal",
		Summary: "Seal exact production membership before member review", MaxBodyBytes: 4096},
		func(ctx context.Context, in *struct {
			SetID    string `path:"set_id" format:"uuid"`
			Revision int64  `path:"revision" minimum:"1"`
			IfMatch  string `header:"If-Match"`
			Body     ProductionMembershipSealRequest
		}) (*struct{ Body ProductionReceipt }, error) {
			etag, err := parseIfMatch(in.IfMatch)
			if err != nil {
				return nil, err
			}
			actor, ok := workspaceSnapshotOwner(ctx)
			if !ok {
				return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
			}
			var receipt redaction.Receipt
			err = g.mutate(func() error {
				var err error
				receipt, err = d.Store.SealProductionMembership(ctx, actor, in.SetID, in.Revision, in.Body.Domain(etag))
				return err
			})
			if err != nil {
				return nil, productionSetError(err)
			}
			return &struct{ Body ProductionReceipt }{Body: ProductionReceipt(receipt)}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "reviewProductionMember", Method: http.MethodPost,
		Path:    "/api/v1/productions/sets/{set_id}/revisions/{revision}/members/{member_id}/review",
		Summary: "Record one exact production member review binding", MaxBodyBytes: 4096},
		func(ctx context.Context, in *struct {
			SetID    string `path:"set_id" format:"uuid"`
			Revision int64  `path:"revision" minimum:"1"`
			MemberID string `path:"member_id" format:"uuid"`
			IfMatch  string `header:"If-Match"`
			Body     ProductionMemberReviewRequest
		}) (*struct{ Body ProductionReceipt }, error) {
			etag, err := parseIfMatch(in.IfMatch)
			if err != nil {
				return nil, err
			}
			actor, ok := workspaceSnapshotOwner(ctx)
			if !ok {
				return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
			}
			var receipt redaction.Receipt
			err = g.mutate(func() error {
				var err error
				receipt, err = d.Store.ReviewProductionMember(ctx, actor, in.SetID, in.Revision,
					in.Body.Domain(etag, in.MemberID))
				return err
			})
			if err != nil {
				return nil, productionSetError(err)
			}
			return &struct{ Body ProductionReceipt }{Body: ProductionReceipt(receipt)}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "forkProductionDraft", Method: http.MethodPost,
		Path:    "/api/v1/productions/sets/{set_id}/revisions/{revision}/fork",
		Summary: "Fork a production revision into a new draft", DefaultStatus: http.StatusCreated,
		MaxBodyBytes: 4096},
		func(ctx context.Context, in *struct {
			SetID    string `path:"set_id" format:"uuid"`
			Revision int64  `path:"revision" minimum:"1"`
			Body     ProductionForkRequest
		}) (*struct{ Body redaction.Draft }, error) {
			actor, ok := workspaceSnapshotOwner(ctx)
			if !ok {
				return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
			}
			var draft redaction.Draft
			err := g.mutate(func() error {
				var err error
				draft, err = d.Store.ForkProductionDraft(ctx, actor, in.SetID, in.Revision, in.Body.OperationID)
				return err
			})
			if err != nil {
				return nil, productionSetError(err)
			}
			return &struct{ Body redaction.Draft }{Body: draft}, nil
		})
}
