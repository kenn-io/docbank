package daemonconn

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/taxrecord"
)

func decodeTaxRecordReceipt(ctx context.Context, response *http.Response, page bool) (api.TaxRecordPage, error) {
	fields := []string{"record_id", "account_id", "filer_party_id", "authority_party_id", "kind", "title", "country", "subdivision", "tax_kind", "form_code", "tax_year", "period_start", "period_end", "due_date", "lead_days", "state", "direction", "amount_minor", "currency", "verification", "verified_on", "verified_revision", "verification_note", "note", "revision", "created_at", "updated_at", "retired_at"}
	nullable := map[string]bool{"account_id": true, "authority_party_id": true, "tax_year": true, "period_start": true, "period_end": true, "due_date": true, "lead_days": true, "amount_minor": true, "verified_on": true, "verified_revision": true, "retired_at": true}
	data, err := readTaxReceipt(ctx, response, page, fields, nullable)
	if err != nil {
		return api.TaxRecordPage{}, err
	}
	var result api.TaxRecordPage
	if page {
		err = json.Unmarshal(data, &result)
	} else {
		var record api.TaxRecord
		err = json.Unmarshal(data, &record)
		result.Items = []api.TaxRecord{record}
	}
	if err != nil {
		return api.TaxRecordPage{}, &responseDecodeError{err: errors.New("invalid tax record response")}
	}
	return result, nil
}

func validTaxRecord(r api.TaxRecord) bool {
	if !validUUIDv4(r.RecordID) || !validUUIDv4(r.FilerPartyID) || (r.AccountID != nil && !validUUIDv4(*r.AccountID)) || (r.AuthorityPartyID != nil && !validUUIDv4(*r.AuthorityPartyID)) || r.Revision < 1 {
		return false
	}
	v := taxrecord.RecordValues{AccountID: r.AccountID, FilerPartyID: r.FilerPartyID, AuthorityPartyID: r.AuthorityPartyID, Kind: r.Kind, Title: r.Title, Country: r.Country, Subdivision: r.Subdivision, TaxKind: r.TaxKind, FormCode: r.FormCode, TaxYear: r.TaxYear, PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, DueDate: r.DueDate, LeadDays: r.LeadDays, State: r.State, Direction: r.Direction, AmountMinor: r.AmountMinor, Currency: r.Currency, Verification: r.Verification, VerifiedOn: r.VerifiedOn, VerifiedRevision: r.VerifiedRevision, VerificationNote: r.VerificationNote, Note: r.Note}
	if taxrecord.ValidateRecord(v) != nil {
		return false
	}
	for _, timestamp := range []*string{&r.CreatedAt, &r.UpdatedAt, r.RetiredAt} {
		if timestamp != nil {
			if _, err := time.Parse(time.RFC3339Nano, *timestamp); err != nil {
				return false
			}
		}
	}
	return true
}
func validateTaxRecordResponse(r *api.TaxRecord, response *http.Response, id string) (api.TaxRecord, error) {
	if r == nil || response == nil || !validTaxRecord(*r) || (id != "" && r.RecordID != id) || response.Header.Get("ETag") != revisionIfMatch(r.Revision) {
		return api.TaxRecord{}, &responseDecodeError{err: errors.New("invalid tax record response")}
	}
	return *r, nil
}
func (c *Connection) TaxRecords(ctx context.Context, filer, country, taxkind string, year *int64, state, kind string, includeRetired bool, limit, offset int) (api.TaxRecordPage, error) {
	if limit < 1 || limit > 1000 || offset < 0 {
		return api.TaxRecordPage{}, errors.New("invalid tax record page")
	}
	l, o := int64(limit), int64(offset)
	var response *http.Response
	_, err := c.apiWithResponse(&response).ListTaxRecords(runtime.WithStreamingResponse(ctx), &apiclient.ListTaxRecordsRequestOptions{Query: &apiclient.ListTaxRecordsQuery{Filer: &filer, Country: &country, Taxkind: &taxkind, Year: year, State: &state, Kind: &kind, IncludeRetired: &includeRetired, Limit: &l, Offset: &o}})
	if err != nil {
		return api.TaxRecordPage{}, safeTaxError(err)
	}
	page, err := decodeTaxRecordReceipt(ctx, response, true)
	if err != nil {
		return api.TaxRecordPage{}, safeTaxError(err)
	}
	if page.Items == nil || page.Limit != limit || page.Offset != offset || len(page.Items) > limit {
		return api.TaxRecordPage{}, &responseDecodeError{err: errors.New("invalid tax record page")}
	}
	for _, r := range page.Items {
		if !validTaxRecord(r) {
			return api.TaxRecordPage{}, &responseDecodeError{err: errors.New("invalid tax record page")}
		}
	}
	return page, nil
}
func (c *Connection) TaxRecord(ctx context.Context, id string) (api.TaxRecord, error) {
	if !validUUIDv4(id) {
		return api.TaxRecord{}, errors.New("invalid tax record ID")
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).GetTaxRecord(runtime.WithStreamingResponse(ctx), &apiclient.GetTaxRecordRequestOptions{PathParams: &apiclient.GetTaxRecordPath{RecordID: id}})
	if err != nil {
		return api.TaxRecord{}, safeTaxError(err)
	}
	receipt, err := decodeTaxRecordReceipt(ctx, response, false)
	if err != nil {
		return api.TaxRecord{}, safeTaxError(err)
	}
	return validateTaxRecordResponse(&receipt.Items[0], response, id)
}
func (c *Connection) CreateTaxRecord(ctx context.Context, in api.CreateTaxRecordRequest) (api.TaxRecord, error) {
	var response *http.Response
	_, err := c.apiWithResponse(&response).CreateTaxRecord(runtime.WithStreamingResponse(ctx), &apiclient.CreateTaxRecordRequestOptions{Body: &in})
	if err != nil {
		return api.TaxRecord{}, taxMutationFailure(mutationRequestError(response, err), "record")
	}
	receipt, err := decodeTaxRecordReceipt(ctx, response, false)
	if err != nil {
		return api.TaxRecord{}, taxMutationFailure(err, "record")
	}
	result, err := validateTaxRecordResponse(&receipt.Items[0], response, "")
	return result, taxMutationFailure(err, "record")
}
func (c *Connection) UpdateTaxRecord(ctx context.Context, id string, revision int64, in api.PatchTaxRecordRequest) (api.TaxRecord, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.TaxRecord{}, errors.New("invalid tax record ID or revision")
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).UpdateTaxRecord(runtime.WithStreamingResponse(ctx), &apiclient.UpdateTaxRecordRequestOptions{PathParams: &apiclient.UpdateTaxRecordPath{RecordID: id}, Header: &apiclient.UpdateTaxRecordHeaders{IfMatch: revisionIfMatch(revision)}, Body: &in})
	if err != nil {
		return api.TaxRecord{}, taxMutationFailure(mutationRequestError(response, err), "record")
	}
	receipt, err := decodeTaxRecordReceipt(ctx, response, false)
	if err != nil {
		return api.TaxRecord{}, taxMutationFailure(err, "record")
	}
	result, err := validateTaxRecordResponse(&receipt.Items[0], response, id)
	return result, taxMutationFailure(err, "record")
}
func (c *Connection) RetireTaxRecord(ctx context.Context, id string, revision int64) (api.TaxRecord, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.TaxRecord{}, errors.New("invalid tax record ID or revision")
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).RetireTaxRecord(runtime.WithStreamingResponse(ctx), &apiclient.RetireTaxRecordRequestOptions{PathParams: &apiclient.RetireTaxRecordPath{RecordID: id}, Header: &apiclient.RetireTaxRecordHeaders{IfMatch: revisionIfMatch(revision)}})
	if err != nil {
		return api.TaxRecord{}, taxMutationFailure(mutationRequestError(response, err), "record")
	}
	receipt, err := decodeTaxRecordReceipt(ctx, response, false)
	if err != nil {
		return api.TaxRecord{}, taxMutationFailure(err, "record")
	}
	result, err := validateTaxRecordResponse(&receipt.Items[0], response, id)
	return result, taxMutationFailure(err, "record")
}
