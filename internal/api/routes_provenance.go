package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

type provenancePageOutput struct{ Body ProvenancePage }

func registerProvenanceRoutes(api huma.API, d Deps) {
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
}
