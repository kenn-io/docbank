package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

type TagGraphSeed struct {
	NodeID    int64  `json:"node_id,omitzero" minimum:"1"`
	TagID     string `json:"tag_id,omitzero" format:"uuid"`
	PassageID string `json:"passage_id,omitzero" maxLength:"128"`
}

type TagNeighborhoodRequest struct {
	Fence           ResolvedDocumentSourceFence `json:"fence"`
	Seed            TagGraphSeed                `json:"seed"`
	AssignmentKinds []string                    `json:"assignment_kinds" minItems:"1" uniqueItems:"true"`
	Limit           int                         `json:"limit,omitzero" minimum:"1" maximum:"100" default:"20"`
	MaxHops         int                         `json:"max_hops,omitzero" minimum:"1" maximum:"10" default:"2"`
	MaxVisited      int                         `json:"max_visited,omitzero" minimum:"1" maximum:"1000" default:"1000"`
}

type TagGraphPathStep struct {
	Kind   string `json:"kind" enum:"document,tag"`
	NodeID int64  `json:"node_id,omitzero" minimum:"1"`
	TagID  string `json:"tag_id,omitzero" format:"uuid"`
}

type TagGraphTag struct {
	ID                  string             `json:"id" format:"uuid"`
	Name                string             `json:"name"`
	ScopedDocumentCount int                `json:"scoped_document_count" minimum:"0"`
	Weight              float64            `json:"weight" minimum:"0"`
	AssignmentOrigin    string             `json:"assignment_origin" enum:"legacy"`
	Path                []TagGraphPathStep `json:"path"`
}

type TagGraphDocument struct {
	NodeID           int64              `json:"node_id" minimum:"1"`
	ContentVersionID string             `json:"content_version_id" format:"uuid"`
	Name             string             `json:"name"`
	Path             string             `json:"path"`
	ModifiedAt       string             `json:"modified_at" format:"date-time"`
	Score            float64            `json:"score" minimum:"0"`
	SharedTags       []TagGraphTag      `json:"shared_tags"`
	GraphPath        []TagGraphPathStep `json:"graph_path"`
}

type TagNeighborhoodResponse struct {
	VaultUID              string             `json:"vault_uid" format:"uuid"`
	DocumentCount         int                `json:"document_count" minimum:"0"`
	UntaggedDocumentCount int                `json:"untagged_document_count" minimum:"0"`
	VisitedNodes          int                `json:"visited_nodes" minimum:"1" maximum:"1000"`
	Truncated             bool               `json:"truncated"`
	Documents             []TagGraphDocument `json:"documents"`
	Tags                  []TagGraphTag      `json:"tags"`
}

type tagNeighborhoodOutput struct {
	Body TagNeighborhoodResponse
}

func registerTagGraphRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "tagNeighborhood", Method: http.MethodPost,
		Path: "/api/v1/tags/neighborhood", Summary: "Find related documents and tags inside an exact source fence",
	}, func(ctx context.Context, in *struct {
		Body TagNeighborhoodRequest
	}) (*tagNeighborhoodOutput, error) {
		result, err := d.Store.TagNeighborhood(ctx, store.TagNeighborhoodRequest{
			Fence: store.TagGraphFence{
				VaultUID: in.Body.Fence.VaultUID, ContentVersionIDs: in.Body.Fence.ContentVersionIDs,
			},
			Seed: store.TagGraphSeed{
				NodeID: in.Body.Seed.NodeID, TagID: in.Body.Seed.TagID, PassageID: in.Body.Seed.PassageID,
			},
			AssignmentKinds: in.Body.AssignmentKinds,
			Limit:           in.Body.Limit,
			MaxHops:         in.Body.MaxHops,
			MaxVisited:      in.Body.MaxVisited,
		})
		if err != nil {
			if errors.Is(err, store.ErrInvalidTagGraph) {
				return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
			}
			return nil, FromStoreError(err)
		}
		return &tagNeighborhoodOutput{Body: fromStoreTagNeighborhood(result)}, nil
	})
}

func fromStoreTagNeighborhood(result store.TagNeighborhood) TagNeighborhoodResponse {
	response := TagNeighborhoodResponse{
		VaultUID: result.VaultUID, DocumentCount: result.DocumentCount,
		UntaggedDocumentCount: result.UntaggedDocumentCount,
		VisitedNodes:          result.VisitedNodes, Truncated: result.Truncated,
		Documents: []TagGraphDocument{}, Tags: []TagGraphTag{},
	}
	for _, document := range result.Documents {
		wire := TagGraphDocument{
			NodeID: document.NodeID, ContentVersionID: document.ContentVersionID,
			Name: document.Name, Path: document.PathName, ModifiedAt: document.ModifiedAt,
			Score: document.Score, SharedTags: []TagGraphTag{},
			GraphPath: fromStoreTagGraphPath(document.Path),
		}
		for _, tag := range document.SharedTags {
			wire.SharedTags = append(wire.SharedTags, fromStoreTagGraphTag(tag))
		}
		response.Documents = append(response.Documents, wire)
	}
	for _, tag := range result.Tags {
		response.Tags = append(response.Tags, fromStoreTagGraphTag(tag))
	}
	return response
}

func fromStoreTagGraphTag(tag store.TagGraphTag) TagGraphTag {
	return TagGraphTag{
		ID: tag.ID, Name: tag.Name, ScopedDocumentCount: tag.ScopedDocumentCount,
		Weight: tag.Weight, AssignmentOrigin: tag.AssignmentOrigin,
		Path: fromStoreTagGraphPath(tag.Path),
	}
}

func fromStoreTagGraphPath(path []store.TagGraphPathStep) []TagGraphPathStep {
	result := make([]TagGraphPathStep, len(path))
	for i, step := range path {
		result[i] = TagGraphPathStep{Kind: step.Kind, NodeID: step.NodeID, TagID: step.TagID}
	}
	return result
}
