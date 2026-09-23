package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

type tagConceptOutput struct {
	ETag string `header:"ETag"`
	Body store.TagConcept
}

type passageTagChangeResponse struct {
	Tag       Tag                   `json:"tag"`
	PassageID string                `json:"passage_id"`
	Ref       document.PassageRefV1 `json:"ref"`
	Changed   bool                  `json:"changed"`
}

func fromStorePassageTagChange(value store.PassageTagChange) passageTagChangeResponse {
	return passageTagChangeResponse{
		Tag: fromStoreTag(value.Tag), PassageID: value.PassageID,
		Ref: value.Ref, Changed: value.Changed,
	}
}

func conceptOutput(value store.TagConcept) *tagConceptOutput {
	return &tagConceptOutput{ETag: fmt.Sprintf("%q", strconv.FormatInt(value.Revision, 10)), Body: value}
}

// RegisterTagConceptRoutes is called by server registration in the shared
// integration change. Every mutation uses the ordinary operation gate.
func RegisterTagConceptRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "getTagConcept", Method: http.MethodGet,
		Path: "/api/v1/tags/{tag_id}/concept", Summary: "Inspect optional concept vocabulary for a tag"},
		func(ctx context.Context, in *struct {
			TagID string `path:"tag_id"`
		}) (*tagConceptOutput, error) {
			value, err := d.Store.ConceptByTagID(ctx, in.TagID)
			if err != nil {
				return nil, FromStoreError(err)
			}
			return conceptOutput(value), nil
		})
	huma.Register(api, huma.Operation{OperationID: "setTagConcept", Method: http.MethodPut,
		Path: "/api/v1/tags/{tag_id}/concept", Summary: "Set concept description with its revision"},
		func(ctx context.Context, in *struct {
			TagID   string `path:"tag_id"`
			IfMatch string `header:"If-Match"`
			Body    struct {
				Description string `json:"description" maxLength:"4096"`
			}
		}) (*tagConceptOutput, error) {
			rev, err := parseIfMatch(in.IfMatch)
			if err != nil {
				return nil, err
			}
			var output *tagConceptOutput
			err = g.mutate(func() error {
				value, err := d.Store.SetTagConcept(ctx, in.TagID, rev, in.Body.Description)
				if err != nil {
					return FromStoreError(err)
				}
				output = conceptOutput(value)
				return nil
			})
			return output, err
		})
	huma.Register(api, huma.Operation{OperationID: "addTagAlias", Method: http.MethodPost,
		Path: "/api/v1/tags/{tag_id}/aliases", Summary: "Add a unique exact alias"},
		func(ctx context.Context, in *struct {
			TagID   string `path:"tag_id"`
			IfMatch string `header:"If-Match"`
			Body    struct {
				Alias string `json:"alias" minLength:"1"`
			}
		}) (*tagConceptOutput, error) {
			rev, err := parseIfMatch(in.IfMatch)
			if err != nil {
				return nil, err
			}
			var output *tagConceptOutput
			err = g.mutate(func() error {
				value, err := d.Store.AddTagAlias(ctx, in.TagID, rev, in.Body.Alias)
				if err != nil {
					return FromStoreError(err)
				}
				output = conceptOutput(value)
				return nil
			})
			return output, err
		})
	huma.Register(api, huma.Operation{OperationID: "removeTagAlias", Method: http.MethodPost,
		Path: "/api/v1/tags/{tag_id}/aliases/remove", Summary: "Remove one exact alias"},
		func(ctx context.Context, in *struct {
			TagID   string `path:"tag_id"`
			IfMatch string `header:"If-Match"`
			Body    struct {
				Alias string `json:"alias" minLength:"1"`
			}
		}) (*tagConceptOutput, error) {
			rev, err := parseIfMatch(in.IfMatch)
			if err != nil {
				return nil, err
			}
			var output *tagConceptOutput
			err = g.mutate(func() error {
				value, err := d.Store.RemoveTagAlias(ctx, in.TagID, rev, in.Body.Alias)
				if err != nil {
					return FromStoreError(err)
				}
				output = conceptOutput(value)
				return nil
			})
			return output, err
		})
	huma.Register(api, huma.Operation{OperationID: "resolveTagConcept", Method: http.MethodGet,
		Path: "/api/v1/tags/resolve-concept", Summary: "Resolve a canonical tag name or unique alias"},
		func(ctx context.Context, in *struct {
			Name string `query:"name" required:"true"`
		}) (*tagOutput, error) {
			tag, err := d.Store.ResolveTagConcept(ctx, in.Name)
			if err != nil {
				return nil, FromStoreError(err)
			}
			return tagResult(tag), nil
		})
	huma.Register(api, huma.Operation{OperationID: "addConceptEdge", Method: http.MethodPost,
		Path: "/api/v1/tag-concepts/edges", Summary: "Connect two existing concept identities"},
		func(ctx context.Context, in *struct {
			Body struct {
				ParentTagID string `json:"parent_tag_id" format:"uuid"`
				ChildTagID  string `json:"child_tag_id" format:"uuid"`
				Kind        string `json:"kind" enum:"broader,related"`
			}
		}) (*struct{ Body store.TagConcept }, error) {
			var output *struct{ Body store.TagConcept }
			err := g.mutate(func() error {
				if err := d.Store.AddConceptEdge(ctx, in.Body.ParentTagID, in.Body.ChildTagID, in.Body.Kind); err != nil {
					return FromStoreError(err)
				}
				value, err := d.Store.ConceptByTagID(ctx, in.Body.ParentTagID)
				if err != nil {
					return FromStoreError(err)
				}
				output = &struct{ Body store.TagConcept }{Body: value}
				return nil
			})
			return output, err
		})
	huma.Register(api, huma.Operation{OperationID: "removeConceptEdge", Method: http.MethodPost,
		Path: "/api/v1/tag-concepts/edges/remove", Summary: "Remove one concept edge"},
		func(ctx context.Context, in *struct {
			Body struct {
				ParentTagID string `json:"parent_tag_id" format:"uuid"`
				ChildTagID  string `json:"child_tag_id" format:"uuid"`
				Kind        string `json:"kind" enum:"broader,related"`
			}
		}) (*struct{ Body store.TagConcept }, error) {
			var output *struct{ Body store.TagConcept }
			err := g.mutate(func() error {
				if err := d.Store.RemoveConceptEdge(ctx, in.Body.ParentTagID, in.Body.ChildTagID, in.Body.Kind); err != nil {
					return FromStoreError(err)
				}
				value, err := d.Store.ConceptByTagID(ctx, in.Body.ParentTagID)
				if err != nil {
					return FromStoreError(err)
				}
				output = &struct{ Body store.TagConcept }{Body: value}
				return nil
			})
			return output, err
		})
	huma.Register(api, huma.Operation{OperationID: "previewTagMerge", Method: http.MethodPost,
		Path: "/api/v1/tag-merges/preview", Summary: "Review all references affected by an identity merge"},
		func(ctx context.Context, in *struct {
			Body struct {
				SourceTagID string `json:"source_tag_id" format:"uuid"`
				TargetTagID string `json:"target_tag_id" format:"uuid"`
			}
		}) (*struct{ Body store.TagMergePreview }, error) {
			value, err := d.Store.PreviewTagMerge(ctx, in.Body.SourceTagID, in.Body.TargetTagID)
			if err != nil {
				return nil, FromStoreError(err)
			}
			return &struct{ Body store.TagMergePreview }{Body: value}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "commitTagMerge", Method: http.MethodPost,
		Path: "/api/v1/tag-merges/commit", Summary: "Apply an explicitly reviewed revision-fenced merge"},
		func(ctx context.Context, in *struct{ Body store.TagMergePreview }) (*struct{ Body store.TagMergeReceipt }, error) {
			var output *struct{ Body store.TagMergeReceipt }
			err := g.mutate(func() error {
				value, err := d.Store.CommitTagMerge(ctx, in.Body)
				if err != nil {
					return FromStoreError(err)
				}
				output = &struct{ Body store.TagMergeReceipt }{Body: value}
				return nil
			})
			return output, err
		})
	huma.Register(api, huma.Operation{OperationID: "reverseTagMerge", Method: http.MethodPost,
		Path: "/api/v1/tag-merges/{merge_id}/reverse", Summary: "Reverse a merge only when original assignments remain distinct"},
		func(ctx context.Context, in *struct {
			MergeID string `path:"merge_id" format:"uuid"`
		}) (*struct{ Body store.TagMergeReceipt }, error) {
			var output *struct{ Body store.TagMergeReceipt }
			err := g.mutate(func() error {
				value, err := d.Store.ReverseTagMerge(ctx, in.MergeID)
				if err != nil {
					return FromStoreError(err)
				}
				output = &struct{ Body store.TagMergeReceipt }{Body: value}
				return nil
			})
			return output, err
		})
	registerPassageTagConceptRoutes(api, d, g)
}

func registerPassageTagConceptRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "assignPassageTag", Method: http.MethodPost,
		Path: "/api/v1/passage-tags", Summary: "Tag one verified exact retained passage"},
		func(ctx context.Context, in *struct {
			Body struct {
				TagID string                `json:"tag_id" format:"uuid"`
				Ref   document.PassageRefV1 `json:"ref"`
			}
		}) (*struct{ Body passageTagChangeResponse }, error) {
			if d.Processing == nil {
				return nil, processingUnavailable()
			}
			if _, err := d.Processing.ResolvePassage(ctx, processing.PassageResolveRequest{
				Ref: in.Body.Ref, MaxBytes: 262144,
			}); err != nil {
				return nil, passageResolveError(err)
			}
			var output *struct{ Body passageTagChangeResponse }
			err := g.mutate(func() error {
				value, err := d.Store.AssignPassageTag(ctx, in.Body.TagID, in.Body.Ref)
				if err != nil {
					return FromStoreError(err)
				}
				output = &struct{ Body passageTagChangeResponse }{Body: fromStorePassageTagChange(value)}
				return nil
			})
			return output, err
		})
	huma.Register(api, huma.Operation{OperationID: "removePassageTag", Method: http.MethodPost,
		Path: "/api/v1/passage-tags/remove", Summary: "Remove a sidecar by its exact immutable passage ID"},
		func(ctx context.Context, in *struct {
			Body struct {
				TagID string                `json:"tag_id" format:"uuid"`
				Ref   document.PassageRefV1 `json:"ref"`
			}
		}) (*struct{ Body passageTagChangeResponse }, error) {
			var output *struct{ Body passageTagChangeResponse }
			err := g.mutate(func() error {
				value, err := d.Store.RemovePassageTag(ctx, in.Body.TagID, in.Body.Ref)
				if err != nil {
					return FromStoreError(err)
				}
				output = &struct{ Body passageTagChangeResponse }{Body: fromStorePassageTagChange(value)}
				return nil
			})
			return output, err
		})
	huma.Register(api, huma.Operation{OperationID: "getPassageTags", Method: http.MethodPost,
		Path: "/api/v1/passage-tags/lookup", Summary: "List tags on an exact retained passage"},
		func(ctx context.Context, in *struct {
			Body struct {
				Ref document.PassageRefV1 `json:"ref"`
			}
		}) (*struct{ Body []Tag }, error) {
			if d.Processing == nil {
				return nil, processingUnavailable()
			}
			if _, err := d.Processing.ResolvePassage(ctx, processing.PassageResolveRequest{
				Ref: in.Body.Ref, MaxBytes: 262144,
			}); err != nil {
				return nil, passageResolveError(err)
			}
			values, err := d.Store.PassageTags(ctx, in.Body.Ref)
			if err != nil {
				return nil, FromStoreError(err)
			}
			result := make([]Tag, 0, len(values))
			for _, tag := range values {
				result = append(result, fromStoreTag(tag))
			}
			return &struct{ Body []Tag }{Body: result}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "passageTagAvailability", Method: http.MethodPost,
		Path: "/api/v1/passage-tags/availability", Summary: "Check whether exact tagged passage authority remains available"},
		func(ctx context.Context, in *struct {
			Body struct {
				Ref document.PassageRefV1 `json:"ref"`
			}
		}) (*struct {
			Body struct {
				Availability string `json:"availability"`
			}
		}, error) {
			if d.Processing == nil {
				return nil, processingUnavailable()
			}
			status := document.PassageTagAvailabilityExact
			if _, err := d.Processing.ResolvePassage(ctx, processing.PassageResolveRequest{
				Ref: in.Body.Ref, MaxBytes: 262144,
			}); errors.Is(err, processing.ErrPassageUnauthorized) || errors.Is(err, processing.ErrPassageUnavailable) {
				status = document.PassageTagAvailabilityUnavailable
			} else if err != nil {
				return nil, passageResolveError(err)
			}
			return &struct {
				Body struct {
					Availability string `json:"availability"`
				}
			}{
				Body: struct {
					Availability string `json:"availability"`
				}{Availability: status},
			}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "tagsByAssignmentMode", Method: http.MethodGet,
		Path: "/api/v1/nodes/{id}/tags-by-mode", Summary: "Select document, passage or either tag assignments"},
		func(ctx context.Context, in *struct {
			ID               int64  `path:"id"`
			ContentVersionID string `query:"content_version_id"`
			Mode             string `query:"mode" enum:"document,passage,either" default:"document"`
		}) (*struct{ Body []Tag }, error) {
			values, err := d.Store.TagsByAssignmentMode(ctx, in.ID, in.ContentVersionID, in.Mode)
			if err != nil {
				return nil, FromStoreError(err)
			}
			result := make([]Tag, 0, len(values))
			for _, tag := range values {
				result = append(result, fromStoreTag(tag))
			}
			return &struct{ Body []Tag }{Body: result}, nil
		})
}
