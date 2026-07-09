package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

type nodeOutput struct {
	ETag string `header:"ETag"`
	Body Node
}

// nodeOutputAt builds the single-node response with a caller-supplied
// display path (used where the store-computed path would mislead, e.g.
// trash responses reporting the pre-trash location).
func nodeOutputAt(n store.Node, path string) *nodeOutput {
	body := fromStoreNode(n)
	body.Path = path
	return &nodeOutput{ETag: fmt.Sprintf("%q", strconv.FormatInt(n.Revision, 10)), Body: body}
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
	return nodeOutputAt(n, p), nil
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

	huma.Register(api, huma.Operation{
		OperationID: "resolvePath", Method: http.MethodGet, Path: "/api/v1/path",
		Summary: "Resolve an absolute virtual path to its node",
		Description: "path is a query parameter (one well-defined encoding; " +
			"catch-all URL segments are ambiguous for encoded slashes). " +
			"Must start with '/'; '/' resolves the root.",
	}, func(ctx context.Context, in *struct {
		Path string `query:"path" required:"true" example:"/inbox/doc.pdf"`
	}) (*nodeOutput, error) {
		if !strings.HasPrefix(in.Path, "/") {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				fmt.Sprintf("path %q must be absolute (start with /)", in.Path))
		}
		n, err := d.Store.NodeByPath(ctx, in.Path)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return nodeWithPath(ctx, d, n.ID)
	})

	type childrenPage struct {
		Body struct {
			Items  []Node `json:"items"`
			Total  int    `json:"total"`
			Limit  int    `json:"limit"`
			Offset int    `json:"offset"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "listChildren", Method: http.MethodGet, Path: "/api/v1/nodes/{id}/children",
		Summary: "List a directory's live children (dirs first, name-sorted), paginated",
	}, func(ctx context.Context, in *struct {
		ID     int64 `path:"id"`
		Limit  int   `query:"limit" default:"500" minimum:"1" maximum:"5000"`
		Offset int   `query:"offset" default:"0" minimum:"0"`
	}) (*childrenPage, error) {
		kids, err := d.Store.Children(ctx, in.ID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &childrenPage{}
		out.Body.Total, out.Body.Limit, out.Body.Offset = len(kids), in.Limit, in.Offset
		out.Body.Items = []Node{}
		for i := in.Offset; i < len(kids) && i < in.Offset+in.Limit; i++ {
			out.Body.Items = append(out.Body.Items, fromStoreNode(kids[i]))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getNodeContent", Method: http.MethodGet, Path: "/api/v1/nodes/{id}/content",
		Summary: "Stream a file's bytes",
	}, func(ctx context.Context, in *struct {
		ID int64 `path:"id"`
	}) (*huma.StreamResponse, error) {
		n, err := d.Store.NodeByID(ctx, in.ID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		if n.IsDir() {
			return nil, NewError(http.StatusUnprocessableEntity, "not_file",
				fmt.Sprintf("node %d is a directory", n.ID))
		}
		f, err := d.Blobs.Open(n.BlobHash)
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal",
				fmt.Sprintf("opening blob %s: %v (run docbank verify)", n.BlobHash, err))
		}
		ct := n.MimeType
		if ct == "" {
			ct = "application/octet-stream"
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			defer func() { _ = f.Close() }()
			hctx.SetHeader("Content-Type", ct)
			hctx.SetHeader("Content-Length", strconv.FormatInt(n.Size, 10))
			_, _ = io.Copy(hctx.BodyWriter(), f)
		}}, nil
	})

	type searchOutput struct {
		Body struct {
			Hits []SearchHit `json:"hits"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "search", Method: http.MethodGet, Path: "/api/v1/search",
		Summary: "Full-text search over node names, best rank first",
	}, func(ctx context.Context, in *struct {
		Q     string `query:"q" required:"true"`
		Limit int    `query:"limit" default:"50" minimum:"1" maximum:"1000"`
	}) (*searchOutput, error) {
		hits, err := d.Store.Search(ctx, in.Q, in.Limit)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &searchOutput{}
		out.Body.Hits = []SearchHit{}
		for _, h := range hits {
			out.Body.Hits = append(out.Body.Hits, SearchHit{Node: fromStoreNode(h.Node), Path: h.Path})
		}
		return out, nil
	})
}
