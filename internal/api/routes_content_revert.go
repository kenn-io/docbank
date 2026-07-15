package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
)

type contentReversionOutput struct {
	ETag string `header:"ETag"`
	Body ContentReversionReceipt
}

func registerContentRevertRoute(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "revertNodeContent", Method: http.MethodPost,
		Path:    "/api/v1/nodes/{id}/revert",
		Summary: "Create a new head from one of the file's prior immutable versions",
		Description: "Reversion is metadata-only: it adopts the source version's existing " +
			"blob authority and records a new content_revert history row under If-Match.",
	}, func(ctx context.Context, in *struct {
		ID      int64  `path:"id" minimum:"1"`
		IfMatch string `header:"If-Match"`
		Body    struct {
			SourceVersionID string `json:"source_version_id" format:"uuid" minLength:"36" maxLength:"36"`
		}
	}) (*contentReversionOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *contentReversionOutput
		err = g.mutate(func() error {
			node, version, source, revertErr := d.Store.RevertContent(
				ctx, in.ID, revision, in.Body.SourceVersionID,
			)
			if revertErr != nil {
				return FromStoreError(revertErr)
			}
			out = &contentReversionOutput{
				ETag: fmt.Sprintf("%q", strconv.FormatInt(node.Revision, 10)),
				Body: ContentReversionReceipt{
					Node:          fromStoreNode(node),
					Version:       fromStoreContentVersion(version),
					SourceVersion: fromStoreContentVersion(source),
				},
			}
			return nil
		})
		return out, err
	})
}
