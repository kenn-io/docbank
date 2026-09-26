package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

type tagOutput struct {
	ETag string `header:"ETag"`
	Body Tag
}
type tagPageOutput struct{ Body TagPage }
type taggedNodePageOutput struct{ Body TaggedNodePage }
type tagDeletionOutput struct {
	ETag string `header:"ETag"`
	Body TagDeletionReceipt
}
type tagAssignmentOutput struct {
	ETag string `header:"ETag"`
	Body TagAssignmentReceipt
}

// Global tag revisions advance for every assignment, including those outside
// a scoped principal's grant. Scoped views use a stable projection revision;
// assignment concurrency instead uses the node revision and ETag.
const scopedTagRevision int64 = 1

func authorizeLocalTagOperation(ctx context.Context, d Deps, operation Operation) error {
	principal, ok := PrincipalFromContext(ctx)
	if !ok || !principal.Local {
		return operationHTTPError(ErrOperationDenied, false)
	}
	_, err := authorizeRequest(ctx, d, operation, nil, false, false)
	return err
}

func authorizeLocalTagMutation(ctx context.Context, d Deps) error {
	return authorizeLocalTagOperation(ctx, d, OperationMetadataMutation)
}

func registerTagRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "listTags", Method: http.MethodGet, Path: "/api/v1/tags",
		Summary: "List tag definitions by name",
	}, func(ctx context.Context, in *struct {
		Limit  int `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset int `query:"offset" default:"0" minimum:"0"`
	}) (*tagPageOutput, error) {
		principal, _ := PrincipalFromContext(ctx)
		if !principal.Local {
			decision, authErr := authorizeRequest(ctx, d, OperationRead, nil, false, false)
			if authErr != nil {
				return nil, authErr
			}
			visible := make(map[string]store.Tag)
			seenNodes := make(map[int64]struct{}, len(decision.SourceIDs))
			for _, sourceID := range decision.SourceIDs {
				version, versionErr := d.Store.ContentVersionByID(ctx, sourceID)
				if versionErr != nil {
					continue
				}
				if _, seen := seenNodes[version.NodeID]; seen {
					continue
				}
				seenNodes[version.NodeID] = struct{}{}
				nodeTags, total, tagsErr := d.Store.NodeTags(ctx, version.NodeID, 1000, 0)
				if tagsErr != nil {
					return nil, FromStoreError(tagsErr)
				}
				if total > len(nodeTags) {
					return nil, NewError(http.StatusRequestEntityTooLarge, "tag_scope_limit",
						"scoped tag projection exceeds 1000 assignments for one source")
				}
				for _, tag := range nodeTags {
					current := visible[tag.ID]
					if current.ID == "" {
						current = tag
						current.Revision = scopedTagRevision
						current.AssignmentCount = 0
					}
					current.AssignmentCount++
					visible[tag.ID] = current
				}
			}
			tags := make([]store.Tag, 0, len(visible))
			for _, tag := range visible {
				tags = append(tags, tag)
			}
			slices.SortFunc(tags, func(a, b store.Tag) int {
				if byName := strings.Compare(a.Name, b.Name); byName != 0 {
					return byName
				}
				return strings.Compare(a.ID, b.ID)
			})
			total := len(tags)
			start := min(in.Offset, total)
			end := min(start+in.Limit, total)
			return tagPage(tags[start:end], total, in.Limit, in.Offset), nil
		}
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
		if err := authorizeLocalTagOperation(ctx, d, OperationRead); err != nil {
			return nil, err
		}
		tag, err := d.Store.TagByName(ctx, in.Name)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return tagResult(tag), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getTag", Method: http.MethodGet, Path: "/api/v1/tags/{tag_id}",
		Summary: "Inspect one tag definition by stable ID",
	}, func(ctx context.Context, in *struct {
		TagID string `path:"tag_id"`
	}) (*tagOutput, error) {
		if err := authorizeLocalTagOperation(ctx, d, OperationRead); err != nil {
			return nil, err
		}
		tag, err := d.Store.TagByID(ctx, in.TagID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return tagResult(tag), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listTagNodes", Method: http.MethodGet,
		Path:    "/api/v1/tags/{tag_id}/nodes",
		Summary: "List live and trashed nodes carrying a tag",
	}, func(ctx context.Context, in *struct {
		TagID    string `path:"tag_id"`
		Limit    int    `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset   int    `query:"offset" default:"0" minimum:"0"`
		LiveOnly bool   `query:"live_only" default:"false"`
	}) (*taggedNodePageOutput, error) {
		if err := authorizeLocalTagOperation(ctx, d, OperationRead); err != nil {
			return nil, err
		}
		var (
			nodes          []store.TaggedNode
			total          int
			omittedTrashed int
			err            error
		)
		if in.LiveOnly {
			nodes, total, omittedTrashed, err =
				d.Store.LiveTaggedNodes(ctx, in.TagID, in.Limit, in.Offset)
		} else {
			nodes, total, err = d.Store.TaggedNodes(ctx, in.TagID, in.Limit, in.Offset)
		}
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &taggedNodePageOutput{Body: TaggedNodePage{
			Items: []TaggedNode{}, Total: total, Limit: in.Limit, Offset: in.Offset,
			OmittedTrashed: omittedTrashed,
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
		if err := authorizeLocalTagOperation(ctx, d, OperationRead); err != nil {
			return nil, err
		}
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
		if err := authorizeLocalTagMutation(ctx, d); err != nil {
			return nil, err
		}
		var out *tagOutput
		err := g.mutate(func() error {
			tag, err := d.Store.CreateTag(ctx, in.Body.Name)
			if err != nil {
				return FromStoreError(err)
			}
			out = tagResult(tag)
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "renameTag", Method: http.MethodPatch, Path: "/api/v1/tags/{tag_id}",
		Summary: "Rename a tag without changing its stable ID",
	}, func(ctx context.Context, in *struct {
		TagID   string `path:"tag_id"`
		IfMatch string `header:"If-Match"`
		Body    struct {
			Name string `json:"name" minLength:"1"`
		}
	}) (*tagOutput, error) {
		if err := authorizeLocalTagMutation(ctx, d); err != nil {
			return nil, err
		}
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *tagOutput
		err = g.mutate(func() error {
			tag, err := d.Store.RenameTag(ctx, in.TagID, revision, in.Body.Name)
			if err != nil {
				return FromStoreError(err)
			}
			out = tagResult(tag)
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "deleteTag", Method: http.MethodDelete, Path: "/api/v1/tags/{tag_id}",
		Summary: "Delete a tag definition and all assignments",
	}, func(ctx context.Context, in *struct {
		TagID   string `path:"tag_id"`
		IfMatch string `header:"If-Match"`
	}) (*tagDeletionOutput, error) {
		if err := authorizeLocalTagMutation(ctx, d); err != nil {
			return nil, err
		}
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *tagDeletionOutput
		err = g.mutate(func() error {
			tag, err := d.Store.DeleteTag(ctx, in.TagID, revision)
			if err != nil {
				return FromStoreError(err)
			}
			out = &tagDeletionOutput{
				ETag: tagETag(tag),
				Body: TagDeletionReceipt{
					Tag: fromStoreTag(tag), RemovedAssignments: tag.AssignmentCount,
				},
			}
			return nil
		})
		return out, err
	})

	registerTagAssignmentRoute(api, d, g, http.MethodPut, true)
	registerTagAssignmentRoute(api, d, g, http.MethodDelete, false)
	registerTagPathAssignmentRoute(api, d, g, http.MethodPut, true)
	registerTagPathAssignmentRoute(api, d, g, http.MethodDelete, false)
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
		if _, err := authorizeRequest(ctx, d, OperationMetadataMutation, nil, false, false); err != nil {
			return nil, err
		}
		node, err := d.Store.NodeByID(ctx, in.ID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		if node.CurrentVersionID == "" {
			principal, ok := PrincipalFromContext(ctx)
			if !ok || !principal.Local {
				return nil, operationHTTPError(ErrOperationDenied, true)
			}
		} else {
			if _, err := authorizeRequest(ctx, d, OperationMetadataMutation, []string{node.CurrentVersionID}, true, true); err != nil {
				return nil, err
			}
		}
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		principal, _ := PrincipalFromContext(ctx)
		var out *tagAssignmentOutput
		err = g.mutate(func() error {
			var scopedCount int
			if !principal.Local {
				decision, authErr := authorizeRequest(ctx, d, OperationMetadataMutation, nil, false, false)
				if authErr != nil {
					return authErr
				}
				if !slices.Contains(decision.SourceIDs, node.CurrentVersionID) {
					return operationHTTPError(ErrOperationDenied, true)
				}
				var countErr error
				scopedCount, countErr = d.Store.CountTagAssignmentsForCurrentVersions(ctx, in.TagID, decision.SourceIDs)
				if countErr != nil {
					return FromStoreError(countErr)
				}
			}
			var change store.TagAssignmentChange
			if assign {
				change, err = d.Store.AssignTag(ctx, in.TagID, in.ID, revision)
			} else {
				change, err = d.Store.UnassignTag(ctx, in.TagID, in.ID, revision)
			}
			if err != nil {
				return FromStoreError(err)
			}
			out = tagAssignmentResult(change)
			if !principal.Local {
				if change.Changed {
					if assign {
						scopedCount++
					} else {
						scopedCount--
					}
				}
				out.Body.Tag.AssignmentCount = scopedCount
				out.Body.Tag.Revision = scopedTagRevision
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	})
}

func registerTagPathAssignmentRoute(api huma.API, d Deps, g *gate, method string, assign bool) {
	operationID, summary := "unassignTagPath", "Remove a tag from a transactionally resolved path"
	if assign {
		operationID, summary = "assignTagPath", "Assign a tag to a transactionally resolved path"
	}
	huma.Register(api, huma.Operation{
		OperationID: operationID, Method: method, Path: "/api/v1/path/tags/{tag_id}",
		Summary: summary,
	}, func(ctx context.Context, in *struct {
		TagID string `path:"tag_id"`
		Body  struct {
			Path string `json:"path" minLength:"1" example:"/records/report.pdf"`
		}
	}) (*tagAssignmentOutput, error) {
		if err := authorizeLocalTagMutation(ctx, d); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(in.Body.Path, "/") {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				fmt.Sprintf("path %q must be absolute (start with /)", in.Body.Path))
		}
		var out *tagAssignmentOutput
		err := g.mutate(func() error {
			var change store.TagAssignmentChange
			var err error
			if assign {
				change, err = d.Store.AssignTagPath(ctx, in.TagID, in.Body.Path)
			} else {
				change, err = d.Store.UnassignTagPath(ctx, in.TagID, in.Body.Path)
			}
			if err != nil {
				return FromStoreError(err)
			}
			out = tagAssignmentResult(change)
			return nil
		})
		return out, err
	})
}

func tagAssignmentResult(change store.TagAssignmentChange) *tagAssignmentOutput {
	wireNode := fromStoreNode(change.Node)
	wireNode.Path = change.Path
	return &tagAssignmentOutput{
		ETag: fmt.Sprintf("%q", strconv.FormatInt(change.Node.Revision, 10)),
		Body: TagAssignmentReceipt{
			Tag: fromStoreTag(change.Tag), Node: wireNode, Changed: change.Changed,
		},
	}
}

func tagResult(tag store.Tag) *tagOutput {
	return &tagOutput{ETag: tagETag(tag), Body: fromStoreTag(tag)}
}

func tagETag(tag store.Tag) string {
	return fmt.Sprintf("%q", strconv.FormatInt(tag.Revision, 10))
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
