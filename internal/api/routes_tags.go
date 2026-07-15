package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

type tagOutput struct{ Body Tag }
type tagPageOutput struct{ Body TagPage }
type taggedNodePageOutput struct{ Body TaggedNodePage }
type tagDeletionOutput struct{ Body TagDeletionReceipt }
type tagAssignmentOutput struct {
	ETag string `header:"ETag"`
	Body TagAssignmentReceipt
}

func registerTagRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "listTags", Method: http.MethodGet, Path: "/api/v1/tags",
		Summary: "List tag definitions by name",
	}, func(ctx context.Context, in *struct {
		Limit  int `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset int `query:"offset" default:"0" minimum:"0"`
	}) (*tagPageOutput, error) {
		tags, total, err := d.Store.Tags(ctx, in.Limit, in.Offset)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return tagPage(tags, total, in.Limit, in.Offset), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "resolveTagByName", Method: http.MethodGet, Path: "/api/v1/tags/by-name",
		Summary: "Resolve an exact tag name to its stable ID",
	}, func(ctx context.Context, in *struct {
		Name string `query:"name" required:"true"`
	}) (*tagOutput, error) {
		tag, err := d.Store.TagByName(ctx, in.Name)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &tagOutput{Body: fromStoreTag(tag)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getTag", Method: http.MethodGet, Path: "/api/v1/tags/{tag_id}",
		Summary: "Inspect one tag definition by stable ID",
	}, func(ctx context.Context, in *struct {
		TagID string `path:"tag_id"`
	}) (*tagOutput, error) {
		tag, err := d.Store.TagByID(ctx, in.TagID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &tagOutput{Body: fromStoreTag(tag)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listTagNodes", Method: http.MethodGet,
		Path:    "/api/v1/tags/{tag_id}/nodes",
		Summary: "List live and trashed nodes carrying a tag",
	}, func(ctx context.Context, in *struct {
		TagID  string `path:"tag_id"`
		Limit  int    `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset int    `query:"offset" default:"0" minimum:"0"`
	}) (*taggedNodePageOutput, error) {
		nodes, total, err := d.Store.TaggedNodes(ctx, in.TagID, in.Limit, in.Offset)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &taggedNodePageOutput{Body: TaggedNodePage{
			Items: []TaggedNode{}, Total: total, Limit: in.Limit, Offset: in.Offset,
		}}
		for _, node := range nodes {
			item := TaggedNode{Node: fromStoreNode(node.Node), Path: node.Path}
			out.Body.Items = append(out.Body.Items, item)
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listNodeTags", Method: http.MethodGet,
		Path:    "/api/v1/nodes/{id}/tags",
		Summary: "List tags assigned to a node",
	}, func(ctx context.Context, in *struct {
		ID     int64 `path:"id"`
		Limit  int   `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset int   `query:"offset" default:"0" minimum:"0"`
	}) (*tagPageOutput, error) {
		tags, total, err := d.Store.NodeTags(ctx, in.ID, in.Limit, in.Offset)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return tagPage(tags, total, in.Limit, in.Offset), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "createTag", Method: http.MethodPost, Path: "/api/v1/tags",
		Summary: "Define a tag with a new stable ID", DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name string `json:"name" minLength:"1"`
		}
	}) (*tagOutput, error) {
		var out *tagOutput
		err := g.mutate(func() error {
			tag, err := d.Store.CreateTag(ctx, in.Body.Name)
			if err != nil {
				return FromStoreError(err)
			}
			out = &tagOutput{Body: fromStoreTag(tag)}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "renameTag", Method: http.MethodPatch, Path: "/api/v1/tags/{tag_id}",
		Summary: "Rename a tag without changing its stable ID",
	}, func(ctx context.Context, in *struct {
		TagID string `path:"tag_id"`
		Body  struct {
			Name string `json:"name" minLength:"1"`
		}
	}) (*tagOutput, error) {
		var out *tagOutput
		err := g.mutate(func() error {
			tag, err := d.Store.RenameTag(ctx, in.TagID, in.Body.Name)
			if err != nil {
				return FromStoreError(err)
			}
			out = &tagOutput{Body: fromStoreTag(tag)}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "deleteTag", Method: http.MethodDelete, Path: "/api/v1/tags/{tag_id}",
		Summary: "Delete a tag definition and all assignments",
	}, func(ctx context.Context, in *struct {
		TagID string `path:"tag_id"`
	}) (*tagDeletionOutput, error) {
		var out *tagDeletionOutput
		err := g.mutate(func() error {
			tag, err := d.Store.DeleteTag(ctx, in.TagID)
			if err != nil {
				return FromStoreError(err)
			}
			out = &tagDeletionOutput{Body: TagDeletionReceipt{
				Tag: fromStoreTag(tag), RemovedAssignments: tag.AssignmentCount,
			}}
			return nil
		})
		return out, err
	})

	registerTagAssignmentRoute(api, d, g, http.MethodPut, true)
	registerTagAssignmentRoute(api, d, g, http.MethodDelete, false)
}

func registerTagAssignmentRoute(api huma.API, d Deps, g *gate, method string, assign bool) {
	operationID, summary := "unassignTag", "Remove a tag assignment from a node"
	if assign {
		operationID, summary = "assignTag", "Assign a tag to a node"
	}
	const path = "/api/v1/nodes/{id}/tags/{tag_id}"
	huma.Register(api, huma.Operation{
		OperationID: operationID, Method: method, Path: path,
		Summary: summary,
	}, func(ctx context.Context, in *struct {
		ID      int64  `path:"id"`
		TagID   string `path:"tag_id"`
		IfMatch string `header:"If-Match"`
	}) (*tagAssignmentOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *tagAssignmentOutput
		err = g.mutate(func() error {
			var node store.Node
			var tag store.Tag
			var changed bool
			if assign {
				node, tag, changed, err = d.Store.AssignTag(ctx, in.TagID, in.ID, revision)
			} else {
				node, tag, changed, err = d.Store.UnassignTag(ctx, in.TagID, in.ID, revision)
			}
			if err != nil {
				return FromStoreError(err)
			}
			wireNode := fromStoreNode(node)
			if node.TrashedAt == nil {
				wireNode.Path, err = d.Store.Path(ctx, node.ID)
				if err != nil {
					return FromStoreError(err)
				}
			}
			out = &tagAssignmentOutput{
				ETag: fmt.Sprintf("%q", strconv.FormatInt(node.Revision, 10)),
				Body: TagAssignmentReceipt{Tag: fromStoreTag(tag), Node: wireNode, Changed: changed},
			}
			return nil
		})
		return out, err
	})
	markDocumentedHeaderRequired(api, path, method, "If-Match")
}

// markDocumentedHeaderRequired keeps Huma's runtime parser permissive enough
// for parseIfMatch to return Docbank's structured 428 response while making
// the actual wire requirement unambiguous to generated clients.
func markDocumentedHeaderRequired(api huma.API, path, method, header string) {
	item := api.OpenAPI().Paths[path]
	var operation *huma.Operation
	switch method {
	case http.MethodPut:
		operation = item.Put
	case http.MethodDelete:
		operation = item.Delete
	default:
		panic("unsupported documented tag assignment method " + method)
	}
	for index, parameter := range operation.Parameters {
		if parameter.In == "header" && parameter.Name == header {
			documentedParameter := *parameter
			documentedParameter.Required = true
			operation.Parameters[index] = &documentedParameter
			return
		}
	}
	panic("tag assignment route lacks documented header " + header)
}

func tagPage(tags []store.Tag, total, limit, offset int) *tagPageOutput {
	out := &tagPageOutput{Body: TagPage{
		Items: []Tag{}, Total: total, Limit: limit, Offset: offset,
	}}
	for _, tag := range tags {
		out.Body.Items = append(out.Body.Items, fromStoreTag(tag))
	}
	return out
}
