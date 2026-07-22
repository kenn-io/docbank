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
// accepted ("3" or 3); anything else — unbalanced or nested quotes included —
// is a 400, not a lenient parse. Empty → 428; garbage or non-positive → 400.
// Non-positive values are rejected because node and tag revisions begin at
// one, and the store reserves -1 as its
// unconditional sentinel: an If-Match of "-1" reaching the store would
// silently skip the precondition this header exists to enforce.
func parseIfMatch(v string) (int64, error) {
	if v == "" {
		return 0, NewError(http.StatusPreconditionRequired, "precondition_required",
			"this endpoint requires If-Match: <revision> (read the target resource to get it)")
	}
	raw := v
	if len(raw) >= 2 && strings.HasPrefix(raw, `"`) && strings.HasSuffix(raw, `"`) {
		raw = raw[1 : len(raw)-1]
	}
	rev, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || rev < 1 {
		return 0, NewError(http.StatusBadRequest, "validation",
			fmt.Sprintf("invalid If-Match %q: want a positive resource revision", v))
	}
	return rev, nil
}

func registerMutateRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "createNode", Method: http.MethodPost, Path: "/api/v1/nodes",
		Summary: "Create a directory", DefaultStatus: http.StatusCreated,
		Description: "kind must be \"dir\"; file creation is POST /api/v1/ingest " +
			"for server-side paths or digest-checked POST /api/v1/uploads for remote bytes.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			ParentID int64  `json:"parent_id"`
			Name     string `json:"name" minLength:"1"`
			Kind     string `json:"kind" enum:"dir,file"`
		}
	}) (*nodeOutput, error) {
		if in.Body.Kind != "dir" {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				"kind \"file\" is not supported here: use POST /api/v1/ingest or POST /api/v1/uploads")
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
			DestPath    *string `json:"dest_path,omitempty"`
		}
	}) (*nodeOutput, error) {
		rev, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		if in.Body.DestPath != nil &&
			(in.Body.NewParentID != nil || in.Body.NewName != nil) {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				"dest_path cannot be combined with new_parent_id or new_name")
		}
		if in.Body.DestPath != nil && !strings.HasPrefix(*in.Body.DestPath, "/") {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				"dest_path must be absolute (start with /)")
		}
		if in.Body.NewParentID == nil && in.Body.NewName == nil && in.Body.DestPath == nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				"nothing to do: set dest_path, new_parent_id, and/or new_name")
		}
		var out *nodeOutput
		err = g.mutate(func() error {
			if in.Body.DestPath != nil {
				n, path, err := d.Store.MoveToPath(ctx, in.ID, rev, *in.Body.DestPath)
				if err != nil {
					return FromStoreError(err)
				}
				out = nodeOutputAt(n, path)
				return nil
			}
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
			n, path, err := d.Store.Move(ctx, in.ID, *parent, name, rev)
			if err != nil {
				return FromStoreError(err)
			}
			out = nodeOutputAt(n, path)
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "movePath", Method: http.MethodPost, Path: "/api/v1/path/move",
		Summary: "Move a node by virtual path in one transaction",
		Description: "CLI-oriented path mutation: source and destination paths " +
			"are resolved inside the daemon/store transaction so ancestor moves " +
			"cannot redirect the operation between separate client requests.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			SrcPath  string `json:"src_path" minLength:"1" example:"/inbox/a.pdf"`
			DestPath string `json:"dest_path" minLength:"1" example:"/filed/a.pdf"`
		}
	}) (*nodeOutput, error) {
		for _, p := range []string{in.Body.SrcPath, in.Body.DestPath} {
			if !strings.HasPrefix(p, "/") {
				return nil, NewError(http.StatusUnprocessableEntity, "validation",
					fmt.Sprintf("path %q must be absolute (start with /)", p))
			}
		}
		var out *nodeOutput
		err := g.mutate(func() error {
			n, path, err := d.Store.MovePath(ctx, in.Body.SrcPath, in.Body.DestPath)
			if err != nil {
				return FromStoreError(err)
			}
			out = nodeOutputAt(n, path)
			return nil
		})
		return out, err
	})

	type batchMoveOutput struct{ Body BatchMoveReport }
	huma.Register(api, huma.Operation{
		OperationID: "batchMove", Method: http.MethodPost, Path: "/api/v1/batch/move",
		Summary: "Apply an all-or-nothing document reorganization",
		Description: "Every source and destination is resolved against one initial tree. " +
			"The daemon validates the complete final tree before changing anything, so plans may " +
			"perform swaps and nested reorganizations without exposing intermediate names. " +
			"A source uses either source_path or node_id plus revision, never both.",
	}, func(ctx context.Context, in *struct {
		Body BatchMoveRequest
	}) (*batchMoveOutput, error) {
		moves := make([]store.BatchMoveRequest, len(in.Body.Moves))
		for index, move := range in.Body.Moves {
			byPath, byID := move.SourcePath != "", move.NodeID != 0
			if byPath == byID {
				return nil, NewError(http.StatusUnprocessableEntity, "invalid_batch_move",
					fmt.Sprintf("move %d source must use exactly one of source_path or node_id", index))
			}
			if byPath {
				if !strings.HasPrefix(move.SourcePath, "/") || move.Revision != 0 {
					return nil, NewError(http.StatusUnprocessableEntity, "invalid_batch_move",
						fmt.Sprintf("move %d path source must be absolute and cannot carry revision", index))
				}
			} else if move.NodeID < 1 || move.Revision < 1 {
				return nil, NewError(http.StatusUnprocessableEntity, "invalid_batch_move",
					fmt.Sprintf("move %d node source requires positive node_id and revision", index))
			}
			if !strings.HasPrefix(move.DestinationPath, "/") {
				return nil, NewError(http.StatusUnprocessableEntity, "invalid_batch_move",
					fmt.Sprintf("move %d destination_path must be absolute", index))
			}
			moves[index] = store.BatchMoveRequest{
				SourcePath: move.SourcePath, NodeID: move.NodeID, IfRevision: move.Revision,
				DestinationPath: move.DestinationPath,
			}
		}
		var output *batchMoveOutput
		err := g.mutate(func() error {
			results, err := d.Store.BatchMove(ctx, moves)
			if err != nil {
				return FromStoreError(err)
			}
			report := BatchMoveReport{Items: make([]BatchMoveReceipt, len(results))}
			for index, result := range results {
				node := fromStoreNode(result.Node)
				node.Path = result.Path
				report.Items[index] = BatchMoveReceipt{FromPath: result.FromPath, Node: node}
			}
			output = &batchMoveOutput{Body: report}
			return nil
		})
		return output, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "trashNode", Method: http.MethodPost, Path: "/api/v1/nodes/{id}/trash",
		Summary: "Move a node and its subtree to the trash",
		Description: "The response's `path` is the node's pre-trash location " +
			"(where a restore would return it), not a resolvable live path.",
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
			// The store computes the pre-trash path inside the trash
			// transaction: afterwards the trash root is re-parented (the
			// path would misleadingly render as a live-looking "/<name>"),
			// and capturing it in a separate call before would race
			// concurrent ancestor moves — mutate() is the gate's shared
			// side, so ordinary mutations run concurrently.
			n, origPath, err := d.Store.Trash(ctx, in.ID, rev)
			if err != nil {
				return FromStoreError(err)
			}
			out = nodeOutputAt(n, origPath)
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "trashPath", Method: http.MethodPost, Path: "/api/v1/path/trash",
		Summary: "Move a virtual path and its subtree to the trash in one transaction",
		Description: "CLI-oriented path mutation: the path is resolved inside " +
			"the daemon/store transaction so ancestor moves cannot redirect a " +
			"separately resolved node id. The response's `path` is the node's " +
			"pre-trash location (where a restore would return it), not a " +
			"resolvable live path.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Path string `json:"path" minLength:"1" example:"/inbox/a.pdf"`
		}
	}) (*nodeOutput, error) {
		if !strings.HasPrefix(in.Body.Path, "/") {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				fmt.Sprintf("path %q must be absolute (start with /)", in.Body.Path))
		}
		var out *nodeOutput
		err := g.mutate(func() error {
			// The canonical pre-trash path comes from inside the trash
			// transaction (see trashNode); the request path may be
			// non-normalized.
			n, origPath, err := d.Store.TrashPath(ctx, in.Body.Path)
			if err != nil {
				return FromStoreError(err)
			}
			out = nodeOutputAt(n, origPath)
			return nil
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
			n, path, err := d.Store.Restore(ctx, in.ID, rev)
			if err != nil {
				return FromStoreError(err)
			}
			out = nodeOutputAt(n, path)
			return nil
		})
		return out, err
	})
}
