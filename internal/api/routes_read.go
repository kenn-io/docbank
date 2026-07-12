package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

func contentDigest(hash []byte) string {
	return "sha-256=:" + base64.StdEncoding.EncodeToString(hash) + ":"
}

func staleRevisionError(id, expected, actual int64) error {
	return FromStoreError(fmt.Errorf("node %d revision is %d, expected %d: %w",
		id, actual, expected, store.ErrStaleRevision))
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
		Description: "X-Docbank-Blob-Hash and X-Docbank-Blob-Size carry catalog identity " +
			"before the body. Content-Digest is an RFC 9530 trailer computed from the bytes " +
			"actually streamed; the standard Content-Length header is omitted so HTTP/1.1 " +
			"can transmit that trailer without a second physical read.",
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Document bytes with expected identity headers and a computed digest trailer",
				Headers: map[string]*huma.Param{
					BlobHashHeader: {Description: "Catalog SHA-256 identity (lowercase hex)",
						Schema: &huma.Schema{Type: "string", Pattern: "^[0-9a-f]{64}$"}},
					BlobSizeHeader: {Description: "Catalog raw byte length",
						Schema: &huma.Schema{Type: "string", Pattern: "^[0-9]+$"}},
					"Content-Digest": {Description: "RFC 9530 SHA-256 of bytes actually streamed; delivered as an HTTP trailer",
						Schema: &huma.Schema{Type: "string"}},
				},
				Content: map[string]*huma.MediaType{
					"application/octet-stream": {Schema: &huma.Schema{Type: "string", Format: "binary"}},
				},
			},
		},
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
		f, err := d.Blobs.OpenContext(ctx, n.BlobHash)
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
			hctx.SetHeader(BlobHashHeader, n.BlobHash)
			hctx.SetHeader(BlobSizeHeader, strconv.FormatInt(n.Size, 10))
			hctx.SetHeader("Trailer", "Content-Digest")
			hash := sha256.New()
			if _, err := io.Copy(hctx.BodyWriter(), io.TeeReader(f, hash)); err == nil {
				hctx.SetHeader("Content-Digest", contentDigest(hash.Sum(nil)))
			}
		}}, nil
	})

	type verifyNodeOutput struct{ Body ContentVerification }
	huma.Register(api, huma.Operation{
		OperationID: "verifyNodeContent", Method: http.MethodPost, Path: "/api/v1/nodes/{id}/verify",
		Summary: "Re-hash one file and bind the evidence to its node revision",
		Description: "Requires If-Match from a prior node response. Returns catalog identity and " +
			"a fresh read through the mixed loose/packed store; a concurrent node change returns 412.",
	}, func(ctx context.Context, in *struct {
		ID      int64  `path:"id"`
		IfMatch string `header:"If-Match"`
	}) (*verifyNodeOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		n, err := d.Store.NodeByID(ctx, in.ID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		if n.IsDir() {
			return nil, NewError(http.StatusUnprocessableEntity, "not_file",
				fmt.Sprintf("node %d is a directory", n.ID))
		}
		if n.Revision != revision {
			return nil, staleRevisionError(n.ID, revision, n.Revision)
		}

		report := ContentVerification{
			NodeID: n.ID, Revision: n.Revision, BlobHash: n.BlobHash, Size: n.Size,
		}
		f, openErr := d.Blobs.OpenContext(ctx, n.BlobHash)
		if openErr != nil {
			if errors.Is(openErr, fs.ErrNotExist) {
				report.Problem = "missing"
			} else {
				report.Problem = "unreadable"
			}
		} else {
			hash := sha256.New()
			report.ComputedSize, err = io.Copy(hash, f)
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				report.Problem = "unreadable"
			} else {
				report.ComputedHash = hex.EncodeToString(hash.Sum(nil))
				report.Verified = report.ComputedHash == n.BlobHash && report.ComputedSize == n.Size
				if !report.Verified {
					report.Problem = "corrupt"
				}
			}
		}

		current, err := d.Store.NodeByID(ctx, in.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, FromStoreError(fmt.Errorf(
					"node %d disappeared during verification: %w", in.ID, store.ErrStaleRevision))
			}
			return nil, FromStoreError(err)
		}
		if current.Revision != revision || current.BlobHash != n.BlobHash {
			return nil, staleRevisionError(n.ID, revision, current.Revision)
		}
		return &verifyNodeOutput{Body: report}, nil
	})

	type searchOutput struct{ Body SearchReport }
	huma.Register(api, huma.Operation{
		OperationID: "search", Method: http.MethodGet, Path: "/api/v1/search",
		Summary: "Full-text search over node names, best rank first",
	}, func(ctx context.Context, in *struct {
		Q     string `query:"q" required:"true"`
		Limit int    `query:"limit" default:"50" minimum:"1" maximum:"1000"`
	}) (*searchOutput, error) {
		hits, truncated, err := d.Store.SearchPage(ctx, in.Q, in.Limit)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &searchOutput{Body: SearchReport{Hits: []SearchHit{}, Limit: in.Limit, Truncated: truncated}}
		for _, h := range hits {
			out.Body.Hits = append(out.Body.Hits, SearchHit{Node: fromStoreNode(h.Node), Path: h.Path})
		}
		return out, nil
	})
}
