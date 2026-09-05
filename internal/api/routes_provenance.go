package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

type provenancePageOutput struct{ Body ProvenancePage }
type provenanceAppendOutput struct {
	ETag string `header:"ETag"`
	Body ProvenanceAppendReceipt
}

func registerProvenanceRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "listNodeProvenance", Method: http.MethodGet,
		Path:    "/api/v1/nodes/{id}/provenance",
		Summary: "List immutable origin facts for one file node",
		Description: "Returns newest-ingest-first provenance, including superseded facts. " +
			"The node and its live path come from the same read snapshot; trashed nodes have no path.",
	}, func(ctx context.Context, in *struct {
		ID     int64 `path:"id" minimum:"1"`
		Limit  int   `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset int   `query:"offset" default:"0" minimum:"0"`
	}) (*provenancePageOutput, error) {
		page, err := d.Store.NodeProvenance(ctx, in.ID, in.Limit, in.Offset)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &provenancePageOutput{Body: ProvenancePage{
			Node: fromStoreNode(page.Node), Items: []ProvenanceFact{},
			Total: page.Total, Limit: page.Limit, Offset: page.Offset,
		}}
		out.Body.Node.Path = page.Path
		for _, fact := range page.Items {
			out.Body.Items = append(out.Body.Items, fromStoreProvenanceFact(fact))
		}
		return out, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "appendNodeProvenance", Method: http.MethodPost,
		Path:    "/api/v1/nodes/{id}/provenance",
		Summary: "Append an immutable origin fact to a file node",
		Description: "Adds post-ingest provenance under the node's If-Match revision. " +
			"The original path remains opaque evidence and is never opened by the daemon.",
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *struct {
		ID      int64  `path:"id" minimum:"1"`
		IfMatch string `header:"If-Match"`
		Body    ProvenanceAppendRequest
	}) (*provenanceAppendOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var result *provenanceAppendOutput
		err = g.mutate(func() error {
			appended, appendErr := d.Store.AppendNodeProvenance(ctx, store.ProvenanceAppendInput{
				NodeID: in.ID, IfRevision: revision, SourceKind: in.Body.SourceKind,
				SourceDescription: in.Body.SourceDescription, OriginalPath: in.Body.OriginalPath,
				OriginalMTime: in.Body.OriginalMTime, Supersedes: in.Body.Supersedes,
			})
			if appendErr != nil {
				return FromStoreError(appendErr)
			}
			result = &provenanceAppendOutput{
				ETag: fmt.Sprintf("%q", strconv.FormatInt(appended.Node.Revision, 10)),
				Body: fromStoreProvenanceAppend(appended),
			}
			return nil
		})
		return result, err
	})
}
