package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
)

type nodeOutput struct {
	ETag string `header:"ETag"`
	Body Node
}

// nodeWithPath loads the node's display path and builds the single-node
// response. Every single-node endpoint returns this shape.
func nodeWithPath(ctx context.Context, d Deps, id int64) (*nodeOutput, error) {
	n, err := d.Store.NodeByID(ctx, id)
	if err != nil {
		return nil, FromStoreError(err)
	}
	p, err := d.Store.Path(ctx, id)
	if err != nil {
		return nil, FromStoreError(err)
	}
	body := fromStoreNode(n)
	body.Path = p
	return &nodeOutput{ETag: fmt.Sprintf("%q", strconv.FormatInt(n.Revision, 10)), Body: body}, nil
}

func registerReadRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "getNode", Method: http.MethodGet, Path: "/api/v1/nodes/{id}",
		Summary: "Stat a node by id (live or trashed)",
	}, func(ctx context.Context, in *struct {
		ID int64 `path:"id"`
	}) (*nodeOutput, error) {
		return nodeWithPath(ctx, d, in.ID)
	})
}
