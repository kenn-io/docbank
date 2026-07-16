package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

func registerContentPruneRoute(api huma.API, d Deps, g *gate) {
	type pruneOutput struct {
		ETag string `header:"ETag"`
		Body VersionPruneReport
	}
	huma.Register(api, huma.Operation{
		OperationID: "pruneNodeContentVersions", Method: http.MethodPost,
		Path:    "/api/v1/nodes/{id}/versions/prune",
		Summary: "Preview or prune selected non-current content versions",
		Description: "Dry-run by default. Execution requires If-Match and releases only logical " +
			"history authority; GC and repack remain separate physical maintenance operations.",
	}, func(ctx context.Context, in *struct {
		ID      int64  `path:"id"`
		IfMatch string `header:"If-Match"`
		Body    VersionPruneRequest
	}) (*pruneOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		age, err := ParseAge(in.Body.OlderThan)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_version_prune", err.Error())
		}
		selector := store.VersionPruneSelector{
			VersionIDs: in.Body.VersionIDs, KeepNewest: in.Body.KeepNewest,
			OlderThan: age, AllPrior: in.Body.AllPrior,
		}
		var report VersionPruneReport
		err = g.mutate(func() error {
			result, pruneErr := d.Store.PruneContentVersions(
				ctx, in.ID, revision, selector, in.Body.Run,
			)
			if pruneErr != nil {
				return FromStoreError(pruneErr)
			}
			report = fromStoreVersionPruneResult(result)
			return nil
		})
		if err != nil {
			return nil, err
		}
		return &pruneOutput{
			ETag: strconv.Quote(strconv.FormatInt(report.Node.Revision, 10)),
			Body: report,
		}, nil
	})
}
