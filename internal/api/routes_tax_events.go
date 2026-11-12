package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

type taxEventMutationOutput struct {
	ETag         string `header:"ETag"`
	CacheControl string `header:"Cache-Control"`
	Body         TaxEventMutation
}
type taxRecordDetailOutput struct {
	ETag         string `header:"ETag"`
	CacheControl string `header:"Cache-Control"`
	Body         TaxRecordDetail
}

func taxEventMutationResult(m store.TaxEventMutation) *taxEventMutationOutput {
	return &taxEventMutationOutput{ETag: revisionETag(m.Record.Revision), CacheControl: cacheControlNoStore, Body: TaxEventMutation{Record: fromStoreTaxRecord(m.Record), Event: fromStoreTaxEvent(m.Event, false)}}
}
func registerTaxEventRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "advanceTaxRecord", Method: http.MethodPost, Path: "/api/v1/tax/records/{record_id}/advance", Summary: "Advance a tax record"}, func(ctx context.Context, in *struct {
		RecordID string `path:"record_id"`
		IfMatch  string `header:"If-Match"`
		Body     AdvanceTaxRecordRequest
	}) (*taxEventMutationOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, taxRouteError(d.Logger, err)
		}
		var out *taxEventMutationOutput
		err = g.mutate(ctx, func() error {
			m, err := d.Store.AdvanceTaxRecord(ctx, in.RecordID, revision, store.TaxEventInput{Kind: in.Body.State, OccurredOn: in.Body.OccurredOn, Currency: in.Body.Currency, Reference: in.Body.Reference, Note: in.Body.Note, Amount: in.Body.Amount})
			if err != nil {
				return err
			}
			out = taxEventMutationResult(m)
			return nil
		})
		return out, taxRouteError(d.Logger, err)
	})
	huma.Register(api, huma.Operation{OperationID: "addTaxEvent", Method: http.MethodPost, Path: "/api/v1/tax/records/{record_id}/events", Summary: "Add a tax record event", DefaultStatus: http.StatusCreated}, func(ctx context.Context, in *struct {
		RecordID string `path:"record_id"`
		IfMatch  string `header:"If-Match"`
		Body     AddTaxEventRequest
	}) (*taxEventMutationOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, taxRouteError(d.Logger, err)
		}
		var out *taxEventMutationOutput
		err = g.mutate(ctx, func() error {
			m, err := d.Store.AddTaxEvent(ctx, in.RecordID, revision, store.TaxEventInput{Kind: in.Body.Kind, OccurredOn: in.Body.OccurredOn, Amount: in.Body.Amount, Currency: in.Body.Currency, Reference: in.Body.Reference, Note: in.Body.Note})
			if err != nil {
				return err
			}
			out = taxEventMutationResult(m)
			return nil
		})
		return out, taxRouteError(d.Logger, err)
	})
	huma.Register(api, huma.Operation{OperationID: "voidTaxEvent", Method: http.MethodPost, Path: "/api/v1/tax/records/{record_id}/events/{event_id}/void", Summary: "Void a tax event"}, func(ctx context.Context, in *struct {
		RecordID string `path:"record_id"`
		EventID  string `path:"event_id"`
		IfMatch  string `header:"If-Match"`
		Body     VoidTaxEventRequest
	}) (*taxEventMutationOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, taxRouteError(d.Logger, err)
		}
		var out *taxEventMutationOutput
		err = g.mutate(ctx, func() error {
			m, err := d.Store.VoidTaxEvent(ctx, in.RecordID, in.EventID, revision, in.Body.Reason)
			if err != nil {
				return err
			}
			out = taxEventMutationResult(m)
			return nil
		})
		return out, taxRouteError(d.Logger, err)
	})
}
