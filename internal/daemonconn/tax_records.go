package daemonconn

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

func validTaxRecord(r api.TaxRecord) bool {
	return validUUIDv4(r.RecordID) && validUUIDv4(r.FilerPartyID) && (r.AccountID == nil || validUUIDv4(*r.AccountID)) && (r.AuthorityPartyID == nil || validUUIDv4(*r.AuthorityPartyID)) && r.Kind != "" && r.Title != "" && r.Country != "" && r.TaxKind != "" && r.State != "" && r.Direction != "" && r.Verification != "" && r.Revision > 0 && r.CreatedAt != "" && r.UpdatedAt != ""
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
	page, err := c.API().ListTaxRecords(ctx, &apiclient.ListTaxRecordsRequestOptions{Query: &apiclient.ListTaxRecordsQuery{Filer: &filer, Country: &country, Taxkind: &taxkind, Year: year, State: &state, Kind: &kind, IncludeRetired: &includeRetired, Limit: &l, Offset: &o}})
	if err != nil {
		return api.TaxRecordPage{}, safeTaxError(err)
	}
	if page == nil || page.Items == nil || page.Limit != limit || page.Offset != offset || len(page.Items) > limit {
		return api.TaxRecordPage{}, &responseDecodeError{err: errors.New("invalid tax record page")}
	}
	for _, r := range page.Items {
		if !validTaxRecord(r) {
			return api.TaxRecordPage{}, &responseDecodeError{err: errors.New("invalid tax record page")}
		}
	}
	return *page, nil
}
func (c *Connection) TaxRecord(ctx context.Context, id string) (api.TaxRecord, error) {
	if !validUUIDv4(id) {
		return api.TaxRecord{}, errors.New("invalid tax record ID")
	}
	var response *http.Response
	r, err := c.apiWithResponse(&response).GetTaxRecord(ctx, &apiclient.GetTaxRecordRequestOptions{PathParams: &apiclient.GetTaxRecordPath{RecordID: id}})
	if err != nil {
		return api.TaxRecord{}, safeTaxError(err)
	}
	return validateTaxRecordResponse(r, response, id)
}
func (c *Connection) CreateTaxRecord(ctx context.Context, in api.CreateTaxRecordRequest) (api.TaxRecord, error) {
	var response *http.Response
	r, err := c.apiWithResponse(&response).CreateTaxRecord(ctx, &apiclient.CreateTaxRecordRequestOptions{Body: &in})
	if err != nil {
		return api.TaxRecord{}, taxMutationFailure(mutationRequestError(response, err), "record")
	}
	result, err := validateTaxRecordResponse(r, response, "")
	return result, taxMutationFailure(err, "record")
}
func (c *Connection) UpdateTaxRecord(ctx context.Context, id string, revision int64, in api.PatchTaxRecordRequest) (api.TaxRecord, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.TaxRecord{}, errors.New("invalid tax record ID or revision")
	}
	var response *http.Response
	r, err := c.apiWithResponse(&response).UpdateTaxRecord(ctx, &apiclient.UpdateTaxRecordRequestOptions{PathParams: &apiclient.UpdateTaxRecordPath{RecordID: id}, Header: &apiclient.UpdateTaxRecordHeaders{IfMatch: revisionIfMatch(revision)}, Body: &in})
	if err != nil {
		return api.TaxRecord{}, taxMutationFailure(mutationRequestError(response, err), "record")
	}
	result, err := validateTaxRecordResponse(r, response, id)
	return result, taxMutationFailure(err, "record")
}
func (c *Connection) RetireTaxRecord(ctx context.Context, id string, revision int64) (api.TaxRecord, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.TaxRecord{}, errors.New("invalid tax record ID or revision")
	}
	var response *http.Response
	r, err := c.apiWithResponse(&response).RetireTaxRecord(ctx, &apiclient.RetireTaxRecordRequestOptions{PathParams: &apiclient.RetireTaxRecordPath{RecordID: id}, Header: &apiclient.RetireTaxRecordHeaders{IfMatch: revisionIfMatch(revision)}})
	if err != nil {
		return api.TaxRecord{}, taxMutationFailure(mutationRequestError(response, err), "record")
	}
	result, err := validateTaxRecordResponse(r, response, id)
	return result, taxMutationFailure(err, "record")
}
