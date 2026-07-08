package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

// parseIfMatch parses the required If-Match revision. ETag-style quoting is
// accepted ("3" or 3). Empty → 428; garbage or negative → 400. Negatives are
// rejected here because the store reserves -1 as its unconditional sentinel:
// an If-Match of "-1" reaching the store would silently skip the precondition
// this header exists to enforce.
func parseIfMatch(v string) (int64, error) {
	if v == "" {
		return 0, NewError(http.StatusPreconditionRequired, "precondition_required",
			"this endpoint requires If-Match: <revision> (stat the node to get it)")
	}
	rev, err := strconv.ParseInt(strings.Trim(v, `"`), 10, 64)
	if err != nil || rev < 0 {
		return 0, NewError(http.StatusBadRequest, "validation",
			fmt.Sprintf("invalid If-Match %q: want a non-negative node revision", v))
	}
	return rev, nil
}

func registerMutateRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "createNode", Method: http.MethodPost, Path: "/api/v1/nodes",
		Summary: "Create a directory", DefaultStatus: http.StatusCreated,
		Description: "kind must be \"dir\"; file creation is POST /api/v1/ingest " +
			"(server-side paths) today, multipart upload later.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			ParentID int64  `json:"parent_id"`
			Name     string `json:"name" minLength:"1"`
			Kind     string `json:"kind" enum:"dir,file"`
		}
	}) (*nodeOutput, error) {
		if in.Body.Kind != "dir" {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				"kind \"file\" is not supported here: use POST /api/v1/ingest (multipart upload is planned)")
		}
		var out *nodeOutput
		err := g.mutate(func() error {
			n, err := d.Store.Mkdir(ctx, in.Body.ParentID, in.Body.Name)
			if err != nil {
				return FromStoreError(err)
			}
			out, err = nodeWithPath(ctx, d, n.ID)
			return err
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "moveNode", Method: http.MethodPatch, Path: "/api/v1/nodes/{id}",
		Summary: "Move and/or rename a node (metadata only; bytes never move)",
	}, func(ctx context.Context, in *struct {
		ID      int64  `path:"id"`
		IfMatch string `header:"If-Match"`
		Body    struct {
			NewParentID *int64  `json:"new_parent_id,omitempty"`
			NewName     *string `json:"new_name,omitempty"`
		}
	}) (*nodeOutput, error) {
		rev, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		if in.Body.NewParentID == nil && in.Body.NewName == nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				"nothing to do: set new_parent_id and/or new_name")
		}
		var out *nodeOutput
		err = g.mutate(func() error {
			// Defaults for the omitted half come from the current node; the
			// revision precondition inside Move catches racing changes.
			cur, err := d.Store.NodeByID(ctx, in.ID)
			if err != nil {
				return FromStoreError(err)
			}
			parent, name := cur.ParentID, cur.Name
			if in.Body.NewParentID != nil {
				parent = in.Body.NewParentID
			}
			if in.Body.NewName != nil {
				name = *in.Body.NewName
			}
			if parent == nil {
				return FromStoreError(fmt.Errorf("node %d: %w", in.ID, store.ErrIsRoot))
			}
			n, err := d.Store.Move(ctx, in.ID, *parent, name, rev)
			if err != nil {
				return FromStoreError(err)
			}
			out, err = nodeWithPath(ctx, d, n.ID)
			return err
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "trashNode", Method: http.MethodPost, Path: "/api/v1/nodes/{id}/trash",
		Summary: "Move a node and its subtree to the trash",
	}, func(ctx context.Context, in *struct {
		ID      int64  `path:"id"`
		IfMatch string `header:"If-Match"`
	}) (*nodeOutput, error) {
		rev, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *nodeOutput
		err = g.mutate(func() error {
			n, err := d.Store.Trash(ctx, in.ID, rev)
			if err != nil {
				return FromStoreError(err)
			}
			out, err = nodeWithPath(ctx, d, n.ID)
			return err
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "restoreNode", Method: http.MethodPost, Path: "/api/v1/nodes/{id}/restore",
		Summary: "Restore a trash root to its original location (root fallback, suffix on collision)",
	}, func(ctx context.Context, in *struct {
		ID      int64  `path:"id"`
		IfMatch string `header:"If-Match"`
	}) (*nodeOutput, error) {
		rev, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *nodeOutput
		err = g.mutate(func() error {
			n, err := d.Store.Restore(ctx, in.ID, rev)
			if err != nil {
				return FromStoreError(err)
			}
			out, err = nodeWithPath(ctx, d, n.ID)
			return err
		})
		return out, err
	})
}
