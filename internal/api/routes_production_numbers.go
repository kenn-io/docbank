package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

func productionNumberLookupError(err error) error {
	if errors.Is(err, production.ErrJobConflict) {
		return NewError(http.StatusConflict, "production_number_conflict",
			"published production number conflicts with retained authority")
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidBatesSelector) {
		return FromStoreError(err)
	}
	return NewError(http.StatusInternalServerError, "production_number_lookup_failed",
		"production number lookup failed")
}

func registerProductionNumberRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{OperationID: "findProductionNumberCandidates", Method: http.MethodGet,
		Path:    "/api/v1/productions/numbers/candidates",
		Summary: "Find bounded verified production number candidates by exact, prefix or substring text"},
		func(ctx context.Context, in *struct {
			Query string `query:"query" maxLength:"256"`
			Limit int    `query:"limit" minimum:"0" maximum:"25"`
		}) (*struct{ Body ProductionNumberCandidates }, error) {
			limit := in.Limit
			if limit == 0 {
				limit = 25
			}
			page, err := d.Store.FindPublishedProductionNumberCandidates(ctx, in.Query, limit)
			if err != nil {
				return nil, productionNumberLookupError(err)
			}
			out := ProductionNumberCandidates{MatchKind: page.MatchKind,
				Items:     make([]ProductionNumberReference, len(page.Items)),
				Ambiguous: page.Ambiguous, Truncated: page.Truncated}
			for index, item := range page.Items {
				out.Items[index] = productionNumberDTO(item)
			}
			return &struct{ Body ProductionNumberCandidates }{Body: out}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "findProductionNumbers", Method: http.MethodGet,
		Path: "/api/v1/productions/numbers", Summary: "Find exact or ranged published production numbers"},
		func(ctx context.Context, in *struct {
			Label         string `query:"label" maxLength:"256"`
			NamespaceID   string `query:"namespace_id"`
			StartSequence int64  `query:"start_sequence" minimum:"0"`
			EndSequence   int64  `query:"end_sequence" minimum:"0"`
			AfterSequence int64  `query:"after_sequence" minimum:"0"`
			Limit         int    `query:"limit" minimum:"0" maximum:"25"`
		}) (*struct{ Body ProductionNumberPage }, error) {
			ranged := in.NamespaceID != "" || in.StartSequence != 0 || in.EndSequence != 0 ||
				in.AfterSequence != 0
			if (in.Label == "") == !ranged || in.Label != "" && in.Limit != 0 {
				return nil, FromStoreError(store.ErrInvalidBatesSelector)
			}
			if in.Label != "" {
				match, err := d.Store.FindPublishedProductionNumber(ctx, in.Label)
				if err != nil {
					return nil, productionNumberLookupError(err)
				}
				return &struct{ Body ProductionNumberPage }{Body: ProductionNumberPage{
					Items: []ProductionNumberReference{productionNumberDTO(match)}}}, nil
			}
			limit := in.Limit
			if limit == 0 {
				limit = 25
			}
			page, err := d.Store.FindPublishedProductionNumberRange(ctx, in.NamespaceID,
				in.StartSequence, in.EndSequence, in.AfterSequence, limit)
			if err != nil {
				return nil, productionNumberLookupError(err)
			}
			out := ProductionNumberPage{Items: make([]ProductionNumberReference, len(page.Items)),
				NextSequence: page.NextSequence}
			for index, item := range page.Items {
				out.Items[index] = productionNumberDTO(item)
			}
			return &struct{ Body ProductionNumberPage }{Body: out}, nil
		})
}
