package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/misslog"
)

func registerMissLogExportRoute(h huma.API, d Deps) {
	type output struct {
		CacheControl string `header:"Cache-Control"`
		Body         misslog.ExportPage
	}
	huma.Register(h, huma.Operation{OperationID: "exportMissLog", Method: http.MethodGet, Path: "/api/v1/miss-log/export", Summary: "Export one bounded page of labeled search examples (operator only)"}, func(ctx context.Context, in *struct {
		Format string `query:"format" enum:"eval,cards"`
		Limit  int    `query:"limit" default:"100" minimum:"1" maximum:"100"`
		Offset int    `query:"offset" default:"0" minimum:"0" maximum:"100000"`
	}) (*output, error) {
		if err := requireMissLogOwner(ctx); err != nil {
			return nil, err
		}
		if in.Format != "eval" && in.Format != "cards" {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", "export format must be eval or cards")
		}
		page, err := d.MissLog.Export(ctx, in.Format, in.Limit, in.Offset)
		if err != nil {
			return nil, missLogFailure()
		}
		return &output{CacheControl: "no-store", Body: page}, nil
	})
}
