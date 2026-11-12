package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

const cacheControlNoStore = "no-store"

type taxAccountOutput struct {
	ETag         string `header:"ETag"`
	CacheControl string `header:"Cache-Control"`
	Body         TaxAccount
}
type taxAccountPageOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         TaxAccountPage
}

func maskTaxIdentifier(value string) string {
	runes := []rune(value)
	if len(runes) == 0 {
		return ""
	}
	if len(runes) <= 4 {
		return "****"
	}
	return "****" + string(runes[len(runes)-4:])
}
func fromStoreTaxAccount(a store.TaxAccount, reveal bool) TaxAccount {
	identifier := a.Identifier
	if !reveal {
		identifier = maskTaxIdentifier(identifier)
	}
	return TaxAccount{AccountID: a.AccountID, FilerPartyID: a.FilerPartyID, AuthorityPartyID: a.AuthorityPartyID, Country: a.Country, Subdivision: a.Subdivision, TaxKind: a.TaxKind, Identifier: identifier, Cadence: a.Cadence, PeriodNote: a.PeriodNote, DueNote: a.DueNote, LeadDays: a.LeadDays, Currency: a.Currency, EffectiveStart: a.EffectiveStart, EffectiveEnd: a.EffectiveEnd, Note: a.Note, Revision: a.Revision, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, RetiredAt: a.RetiredAt}
}
func taxAccountResult(a store.TaxAccount, reveal bool) *taxAccountOutput {
	return &taxAccountOutput{ETag: revisionETag(a.Revision), Body: fromStoreTaxAccount(a, reveal)}
}
func mergeTaxAccountPatch(a store.TaxAccount, p PatchTaxAccountRequest) store.TaxAccountInput {
	in := store.TaxAccountInput{FilerPartyID: a.FilerPartyID, AuthorityPartyID: a.AuthorityPartyID, Country: a.Country, Subdivision: a.Subdivision, TaxKind: a.TaxKind, Identifier: a.Identifier, Cadence: a.Cadence, PeriodNote: a.PeriodNote, DueNote: a.DueNote, LeadDays: new(a.LeadDays), Currency: a.Currency, EffectiveStart: a.EffectiveStart, EffectiveEnd: a.EffectiveEnd, Note: a.Note}
	if p.Identifier != nil {
		in.Identifier = *p.Identifier
	}
	if p.Cadence != nil {
		in.Cadence = *p.Cadence
	}
	if p.PeriodNote != nil {
		in.PeriodNote = *p.PeriodNote
	}
	if p.DueNote != nil {
		in.DueNote = *p.DueNote
	}
	if p.LeadDays != nil {
		in.LeadDays = p.LeadDays
	}
	if p.Currency != nil {
		in.Currency = *p.Currency
	}
	if p.Note != nil {
		in.Note = *p.Note
	}
	if p.startPresent || p.ClearEffectiveStart {
		in.EffectiveStart = p.EffectiveStart
	}
	if p.endPresent || p.ClearEffectiveEnd {
		in.EffectiveEnd = p.EffectiveEnd
	}
	return in
}
func registerTaxAccountRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "listTaxAccounts", Method: http.MethodGet, Path: "/api/v1/tax/accounts", Summary: "List tax accounts"}, func(ctx context.Context, in *struct {
		Filer          string `query:"filer"`
		Country        string `query:"country"`
		TaxKind        string `query:"taxkind"`
		IncludeRetired bool   `query:"include_retired"`
		Limit          int    `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset         int    `query:"offset" default:"0" minimum:"0"`
	}) (*taxAccountPageOutput, error) {
		country := []byte(in.Country)
		for i, c := range country {
			if c >= 'a' && c <= 'z' {
				country[i] = c - ('a' - 'A')
			}
		}
		rows, err := d.Store.ListTaxAccounts(ctx, store.TaxAccountListOptions{FilerPartyID: in.Filer, Country: string(country), TaxKind: in.TaxKind, IncludeRetired: in.IncludeRetired, Limit: in.Limit, Offset: in.Offset})
		if err != nil {
			return nil, taxRouteError(err)
		}
		out := &taxAccountPageOutput{CacheControl: cacheControlNoStore, Body: TaxAccountPage{Items: []TaxAccount{}, Limit: in.Limit, Offset: in.Offset}}
		for _, a := range rows {
			out.Body.Items = append(out.Body.Items, fromStoreTaxAccount(a, false))
		}
		return out, nil
	})
	huma.Register(api, huma.Operation{OperationID: "getTaxAccount", Method: http.MethodGet, Path: "/api/v1/tax/accounts/{account_id}", Summary: "Inspect a tax account"}, func(ctx context.Context, in *struct {
		AccountID string `path:"account_id"`
		Reveal    bool   `query:"reveal"`
	}) (*taxAccountOutput, error) {
		a, err := d.Store.GetTaxAccount(ctx, in.AccountID)
		if err != nil {
			return nil, taxRouteError(err)
		}
		out := taxAccountResult(a, in.Reveal)
		out.CacheControl = cacheControlNoStore
		return out, nil
	})
	huma.Register(api, huma.Operation{OperationID: "createTaxAccount", Method: http.MethodPost, Path: "/api/v1/tax/accounts", Summary: "Create a tax account", DefaultStatus: http.StatusCreated}, func(ctx context.Context, in *struct{ Body CreateTaxAccountRequest }) (*taxAccountOutput, error) {
		var out *taxAccountOutput
		err := g.mutate(ctx, func() error {
			a, err := d.Store.CreateTaxAccount(ctx, in.Body.input())
			if err != nil {
				return err
			}
			out = taxAccountResult(a, false)
			return nil
		})
		return out, taxRouteError(err)
	})
	huma.Register(api, huma.Operation{OperationID: "updateTaxAccount", Method: http.MethodPatch, Path: "/api/v1/tax/accounts/{account_id}", Summary: "Edit a tax account"}, func(ctx context.Context, in *struct {
		AccountID string `path:"account_id"`
		IfMatch   string `header:"If-Match"`
		Body      PatchTaxAccountRequest
	}) (*taxAccountOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, taxRouteError(err)
		}
		var out *taxAccountOutput
		err = g.mutate(ctx, func() error {
			a, err := d.Store.GetTaxAccount(ctx, in.AccountID)
			if err != nil {
				return err
			}
			input := mergeTaxAccountPatch(a, in.Body)
			a, err = d.Store.UpdateTaxAccount(ctx, in.AccountID, revision, input)
			if err != nil {
				return err
			}
			out = taxAccountResult(a, false)
			return nil
		})
		return out, taxRouteError(err)
	})
	huma.Register(api, huma.Operation{OperationID: "retireTaxAccount", Method: http.MethodPost, Path: "/api/v1/tax/accounts/{account_id}/retire", Summary: "Retire a tax account"}, func(ctx context.Context, in *struct {
		AccountID string `path:"account_id"`
		IfMatch   string `header:"If-Match"`
	}) (*taxAccountOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, taxRouteError(err)
		}
		var out *taxAccountOutput
		err = g.mutate(ctx, func() error {
			a, err := d.Store.RetireTaxAccount(ctx, in.AccountID, revision)
			if err != nil {
				return err
			}
			out = taxAccountResult(a, false)
			return nil
		})
		return out, taxRouteError(err)
	})
}
