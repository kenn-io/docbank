package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/taxrecord"
)

type taxRecordOutput struct {
	ETag         string `header:"ETag"`
	CacheControl string `header:"Cache-Control"`
	Body         TaxRecord
}
type taxRecordPageOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         TaxRecordPage
}

func fromStoreTaxRecord(r store.TaxRecord) TaxRecord {
	return TaxRecord{RecordID: r.RecordID, AccountID: r.AccountID, FilerPartyID: r.FilerPartyID, AuthorityPartyID: r.AuthorityPartyID, Kind: r.Kind, Title: r.Title, Country: r.Country, Subdivision: r.Subdivision, TaxKind: r.TaxKind, FormCode: r.FormCode, TaxYear: r.TaxYear, PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, DueDate: r.DueDate, LeadDays: r.LeadDays, State: r.State, Direction: r.Direction, AmountMinor: r.AmountMinor, Currency: r.Currency, Verification: r.Verification, VerifiedOn: r.VerifiedOn, VerifiedRevision: r.VerifiedRevision, VerificationNote: r.VerificationNote, Note: r.Note, Revision: r.Revision, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, RetiredAt: r.RetiredAt}
}
func taxRecordResult(r store.TaxRecord) *taxRecordOutput {
	return &taxRecordOutput{ETag: revisionETag(r.Revision), Body: fromStoreTaxRecord(r)}
}
func taxAmountText(minor *int64, code string) (*string, error) {
	if minor == nil {
		return nil, nil
	}
	s, err := taxrecord.FormatMoney(*minor, code)
	if err != nil {
		return nil, err
	}
	return &s, nil
}
func mergeTaxRecordPatch(r store.TaxRecord, p PatchTaxRecordRequest) (store.TaxRecordInput, error) {
	in := store.TaxRecordInput{AccountID: r.AccountID, Kind: r.Kind, Title: r.Title, FormCode: r.FormCode, TaxYear: r.TaxYear, PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, DueDate: r.DueDate, LeadDays: r.LeadDays, Direction: r.Direction, Note: r.Note, AuthorityPartyID: r.AuthorityPartyID}
	if p.Currency != nil {
		code, err := taxrecord.NormalizeCurrency(*p.Currency)
		if err != nil {
			return in, err
		}
		if code != r.Currency && r.AmountMinor != nil && p.Amount == nil && !p.present["amount"] {
			return in, &taxrecord.FieldError{Field: "amount", Reason: "amount required when currency changes"}
		}
	}
	var err error
	in.Amount, err = taxAmountText(r.AmountMinor, r.Currency)
	if err != nil {
		return in, err
	}
	if p.FilerPartyID != nil {
		in.FilerPartyID = p.FilerPartyID
	}
	if p.Country != nil {
		in.Country = p.Country
	}
	if p.Subdivision != nil {
		in.Subdivision = p.Subdivision
	}
	if p.TaxKind != nil {
		in.TaxKind = p.TaxKind
	}
	if p.Currency != nil {
		in.Currency = p.Currency
	}
	if p.Title != nil {
		in.Title = *p.Title
	}
	if p.FormCode != nil {
		in.FormCode = *p.FormCode
	}
	if p.Direction != nil {
		in.Direction = *p.Direction
	}
	if p.Note != nil {
		in.Note = *p.Note
	}
	if p.AuthorityPartyID != nil || p.present["authority_party_id"] {
		in.AuthorityPartyID = p.AuthorityPartyID
		in.AuthoritySet = true
	}
	if p.TaxYear != nil || p.present["tax_year"] {
		in.TaxYear = p.TaxYear
	}
	if p.PeriodStart != nil || p.present["period_start"] {
		in.PeriodStart = p.PeriodStart
	}
	if p.PeriodEnd != nil || p.present["period_end"] {
		in.PeriodEnd = p.PeriodEnd
	}
	if p.DueDate != nil || p.present["due_date"] {
		in.DueDate = p.DueDate
	}
	if p.LeadDays != nil || p.present["lead_days"] {
		in.LeadDays = p.LeadDays
	}
	if p.Amount != nil || p.present["amount"] {
		in.Amount = p.Amount
	}
	return in, nil
}
func registerTaxRecordRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{OperationID: "listTaxRecords", Method: http.MethodGet, Path: "/api/v1/tax/records", Summary: "List tax records"}, func(ctx context.Context, in *struct {
		Filer          string `query:"filer"`
		Country        string `query:"country"`
		TaxKind        string `query:"taxkind"`
		Year           int64  `query:"year" minimum:"1" maximum:"9999"`
		State          string `query:"state"`
		Kind           string `query:"kind"`
		IncludeRetired bool   `query:"include_retired"`
		Limit          int    `query:"limit" default:"100" minimum:"1" maximum:"1000"`
		Offset         int    `query:"offset" default:"0" minimum:"0"`
	}) (*taxRecordPageOutput, error) {
		var year *int64
		if in.Year != 0 {
			year = &in.Year
		}
		rows, err := d.Store.ListTaxRecords(ctx, store.TaxRecordListOptions{FilerPartyID: in.Filer, Country: taxCountrySelector(in.Country), TaxKind: in.TaxKind, TaxYear: year, State: in.State, Kind: in.Kind, IncludeRetired: in.IncludeRetired, Limit: in.Limit, Offset: in.Offset})
		if err != nil {
			return nil, taxRouteError(d.Logger, err)
		}
		out := &taxRecordPageOutput{CacheControl: cacheControlNoStore, Body: TaxRecordPage{Items: []TaxRecord{}, Limit: in.Limit, Offset: in.Offset}}
		for _, r := range rows {
			out.Body.Items = append(out.Body.Items, fromStoreTaxRecord(r))
		}
		return out, nil
	})
	huma.Register(api, huma.Operation{OperationID: "getTaxRecord", Method: http.MethodGet, Path: "/api/v1/tax/records/{record_id}", Summary: "Inspect a tax record"}, func(ctx context.Context, in *struct {
		RecordID string `path:"record_id"`
	}) (*taxRecordOutput, error) {
		r, err := d.Store.GetTaxRecord(ctx, in.RecordID)
		if err != nil {
			return nil, taxRouteError(d.Logger, err)
		}
		out := taxRecordResult(r)
		out.CacheControl = cacheControlNoStore
		return out, nil
	})
	huma.Register(api, huma.Operation{OperationID: "createTaxRecord", Method: http.MethodPost, Path: "/api/v1/tax/records", Summary: "Create a tax record", DefaultStatus: http.StatusCreated}, func(ctx context.Context, in *struct{ Body CreateTaxRecordRequest }) (*taxRecordOutput, error) {
		var out *taxRecordOutput
		err := g.mutate(ctx, func() error {
			r, err := d.Store.CreateTaxRecord(ctx, in.Body.input())
			if err != nil {
				return err
			}
			out = taxRecordResult(r)
			return nil
		})
		return out, taxRouteError(d.Logger, err)
	})
	huma.Register(api, huma.Operation{OperationID: "updateTaxRecord", Method: http.MethodPatch, Path: "/api/v1/tax/records/{record_id}", Summary: "Edit a tax record"}, func(ctx context.Context, in *struct {
		RecordID string `path:"record_id"`
		IfMatch  string `header:"If-Match"`
		Body     PatchTaxRecordRequest
	}) (*taxRecordOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, taxRouteError(d.Logger, err)
		}
		var out *taxRecordOutput
		err = g.mutate(ctx, func() error {
			r, err := d.Store.GetTaxRecord(ctx, in.RecordID)
			if err != nil {
				return err
			}
			if r.Revision != revision {
				return store.ErrStaleRevision
			}
			input, err := mergeTaxRecordPatch(r, in.Body)
			if err != nil {
				return err
			}
			r, err = d.Store.UpdateTaxRecord(ctx, in.RecordID, revision, input)
			if err != nil {
				return err
			}
			out = taxRecordResult(r)
			return nil
		})
		return out, taxRouteError(d.Logger, err)
	})
	huma.Register(api, huma.Operation{OperationID: "retireTaxRecord", Method: http.MethodPost, Path: "/api/v1/tax/records/{record_id}/retire", Summary: "Retire a tax record"}, func(ctx context.Context, in *struct {
		RecordID string `path:"record_id"`
		IfMatch  string `header:"If-Match"`
	}) (*taxRecordOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, taxRouteError(d.Logger, err)
		}
		var out *taxRecordOutput
		err = g.mutate(ctx, func() error {
			r, err := d.Store.RetireTaxRecord(ctx, in.RecordID, revision)
			if err != nil {
				return err
			}
			out = taxRecordResult(r)
			return nil
		})
		return out, taxRouteError(d.Logger, err)
	})
}
