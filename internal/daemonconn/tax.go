package daemonconn

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/taxrecord"
)

// taxClientError keeps transport and response classification without printing request values.
type taxClientError struct{ cause error }

func (e *taxClientError) Error() string {
	message := "tax request failed"
	switch {
	case errors.Is(e.cause, context.Canceled):
		message += ": canceled"
	case errors.Is(e.cause, context.DeadlineExceeded):
		message += ": deadline exceeded"
	case IsTransportError(e.cause):
		message += ": transport failure"
	case IsResponseDecodeError(e.cause):
		message += ": invalid response"
	}
	if status, ok := ResponseStatus(e.cause); ok {
		message = fmt.Sprintf("tax request failed (HTTP %d)", status)
	}
	if code, ok := ProblemCode(e.cause); ok {
		switch code {
		case "validation", "invalid_tax", "not_found", "exists", "stale_revision", "precondition_required", "tax_retired", "tax_revision_exhausted", "audit_mutation_unsupported", "maintenance_busy", "internal":
			message += ": " + code
			if code == "audit_mutation_unsupported" {
				message += "; tax writes are unavailable in audited vaults"
			}
			if code == "invalid_tax" || code == "validation" {
				if problem, ok := errors.AsType[*problemError](e.cause); ok {
					if detail := api.SafeTaxDiagnostic(problem.Error()); detail != "" {
						message += "; " + detail
					}
				}
			}
		}
	}
	return message
}
func (e *taxClientError) Unwrap() error { return e.cause }
func safeTaxError(err error) error {
	if err == nil {
		return nil
	}
	return &taxClientError{cause: err}
}

type taxMutationError struct{ cause error }

func (e *taxMutationError) Error() string {
	return e.cause.Error() + "; the change may have been applied; inspect the account list or show the account before trying again"
}
func (e *taxMutationError) Unwrap() error { return e.cause }
func taxMutationFailure(err error) error {
	if err == nil {
		return nil
	}
	err = safeTaxError(err)
	if IsRetryableMutationError(err) {
		return &taxMutationError{cause: err}
	}
	return err
}

// A maximum page has 1,000 accounts. Three 8,192-byte notes, two
// 256-byte fields and location codes expand by at most six JSON bytes per
// source byte; 4,096 bytes per row covers names, IDs, dates and framing.
// The remaining space covers the page envelope and harmless metadata.
const maxTaxReceiptBytes = 1000*(6*(3*8192+2*256+2+6+3)+4096) + 1024*1024

// decodeTaxReceipt checks wire presence before zero values can hide omissions.
func decodeTaxReceipt(ctx context.Context, response *http.Response, page bool) (api.TaxAccountPage, error) {
	return decodeTaxReceiptLimit(ctx, response, page, maxTaxReceiptBytes)
}

func decodeTaxReceiptLimit(ctx context.Context, response *http.Response, page bool, limit int64) (api.TaxAccountPage, error) {
	defer func() { _ = response.Body.Close() }()
	if err := ctx.Err(); err != nil {
		return api.TaxAccountPage{}, &responseDecodeError{err: err}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return api.TaxAccountPage{}, &responseDecodeError{err: err}
	}
	if int64(len(data)) > limit {
		return api.TaxAccountPage{}, &responseDecodeError{err: errors.New("tax account response exceeds size limit")}
	}
	invalid := func() (api.TaxAccountPage, error) {
		return api.TaxAccountPage{}, &responseDecodeError{err: errors.New("invalid tax account response")}
	}
	var items []jsontext.Value
	if page {
		var envelope map[string]jsontext.Value
		if json.Unmarshal(data, &envelope) != nil || envelope["items"] == nil || envelope["limit"] == nil || envelope["offset"] == nil || string(envelope["limit"]) == "null" || string(envelope["offset"]) == "null" || json.Unmarshal(envelope["items"], &items) != nil || items == nil {
			return invalid()
		}
	} else {
		items = []jsontext.Value{data}
	}
	for _, item := range items {
		var members map[string]jsontext.Value
		if json.Unmarshal(item, &members) != nil || members == nil {
			return invalid()
		}
		for _, field := range []string{"account_id", "filer_party_id", "authority_party_id", "country", "subdivision", "tax_kind", "identifier", "cadence", "period_note", "due_note", "lead_days", "currency", "effective_start", "effective_end", "note", "revision", "created_at", "updated_at", "retired_at"} {
			value, ok := members[field]
			if !ok {
				return invalid()
			}
			if string(value) == "null" && field != "authority_party_id" && field != "effective_start" && field != "effective_end" && field != "retired_at" {
				return invalid()
			}
		}
	}
	var result api.TaxAccountPage
	if page {
		err = json.Unmarshal(data, &result)
	} else {
		var account api.TaxAccount
		err = json.Unmarshal(data, &account)
		result.Items = []api.TaxAccount{account}
	}
	if err != nil {
		return invalid()
	}
	return result, nil
}

func validTaxAccount(a api.TaxAccount) bool {
	if !validUUIDv4(a.AccountID) || !validUUIDv4(a.FilerPartyID) || (a.AuthorityPartyID != nil && !validUUIDv4(*a.AuthorityPartyID)) || a.Revision < 1 {
		return false
	}
	values := taxrecord.AccountValues{FilerPartyID: a.FilerPartyID, AuthorityPartyID: a.AuthorityPartyID, Country: a.Country, Subdivision: a.Subdivision, TaxKind: a.TaxKind, Cadence: a.Cadence, PeriodNote: a.PeriodNote, DueNote: a.DueNote, LeadDays: a.LeadDays, Currency: a.Currency, EffectiveStart: a.EffectiveStart, EffectiveEnd: a.EffectiveEnd, Note: a.Note}
	if taxrecord.ValidateAccount(values) != nil {
		return false
	}
	for _, timestamp := range []*string{&a.CreatedAt, &a.UpdatedAt, a.RetiredAt} {
		if timestamp != nil {
			if _, err := time.Parse(time.RFC3339Nano, *timestamp); err != nil {
				return false
			}
		}
	}
	return true
}
func validateTaxAccountResponse(a *api.TaxAccount, response *http.Response, id string) (api.TaxAccount, error) {
	if a == nil || response == nil || !validTaxAccount(*a) || (id != "" && a.AccountID != id) || response.Header.Get("ETag") != revisionIfMatch(a.Revision) {
		return api.TaxAccount{}, &responseDecodeError{err: errors.New("invalid tax account response")}
	}
	return *a, nil
}

// A successful mutation advances exactly one revision and has the requested retirement state.
func validateTaxMutationResponse(a *api.TaxAccount, response *http.Response, id string, previous int64, retired bool) (api.TaxAccount, error) {
	result, err := validateTaxAccountResponse(a, response, id)
	if err == nil && (result.Revision-1 != previous || (result.RetiredAt != nil) != retired) {
		return api.TaxAccount{}, taxMutationFailure(&responseDecodeError{err: errors.New("invalid tax account mutation response")})
	}
	return result, taxMutationFailure(err)
}
func (c *Connection) TaxAccounts(ctx context.Context, filer, country, taxkind string, includeRetired bool, limit, offset int) (api.TaxAccountPage, error) {
	if limit < 1 || limit > 1000 || offset < 0 {
		return api.TaxAccountPage{}, errors.New("invalid tax account page")
	}
	queryLimit, queryOffset := int64(limit), int64(offset)
	var response *http.Response
	_, err := c.apiWithResponse(&response).ListTaxAccounts(runtime.WithStreamingResponse(ctx), &apiclient.ListTaxAccountsRequestOptions{Query: &apiclient.ListTaxAccountsQuery{Filer: &filer, Country: &country, Taxkind: &taxkind, IncludeRetired: &includeRetired, Limit: &queryLimit, Offset: &queryOffset}})
	if err != nil {
		return api.TaxAccountPage{}, safeTaxError(err)
	}
	page, err := decodeTaxReceipt(ctx, response, true)
	if err != nil {
		return api.TaxAccountPage{}, safeTaxError(err)
	}
	if page.Items == nil || page.Limit != limit || page.Offset != offset || len(page.Items) > limit {
		return api.TaxAccountPage{}, &responseDecodeError{err: errors.New("invalid tax account page")}
	}
	for _, a := range page.Items {
		if !validTaxAccount(a) {
			return api.TaxAccountPage{}, &responseDecodeError{err: errors.New("invalid tax account page")}
		}
	}
	return page, nil
}
func (c *Connection) TaxAccount(ctx context.Context, id string, reveal bool) (api.TaxAccount, error) {
	if !validUUIDv4(id) {
		return api.TaxAccount{}, errors.New("invalid tax account ID")
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).GetTaxAccount(runtime.WithStreamingResponse(ctx), &apiclient.GetTaxAccountRequestOptions{PathParams: &apiclient.GetTaxAccountPath{AccountID: id}, Query: &apiclient.GetTaxAccountQuery{Reveal: &reveal}})
	if err != nil {
		return api.TaxAccount{}, safeTaxError(err)
	}
	receipt, err := decodeTaxReceipt(ctx, response, false)
	if err != nil {
		return api.TaxAccount{}, safeTaxError(err)
	}
	return validateTaxAccountResponse(&receipt.Items[0], response, id)
}
func (c *Connection) CreateTaxAccount(ctx context.Context, in api.CreateTaxAccountRequest) (api.TaxAccount, error) {
	var response *http.Response
	_, err := c.apiWithResponse(&response).CreateTaxAccount(runtime.WithStreamingResponse(ctx), &apiclient.CreateTaxAccountRequestOptions{Body: &in})
	if err != nil {
		return api.TaxAccount{}, taxMutationFailure(mutationRequestError(response, err))
	}
	receipt, err := decodeTaxReceipt(ctx, response, false)
	if err != nil {
		return api.TaxAccount{}, taxMutationFailure(err)
	}
	return validateTaxMutationResponse(&receipt.Items[0], response, "", 0, false)
}
func (c *Connection) UpdateTaxAccount(ctx context.Context, id string, revision int64, in api.PatchTaxAccountRequest) (api.TaxAccount, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.TaxAccount{}, errors.New("invalid tax account ID or revision")
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).UpdateTaxAccount(runtime.WithStreamingResponse(ctx), &apiclient.UpdateTaxAccountRequestOptions{PathParams: &apiclient.UpdateTaxAccountPath{AccountID: id}, Header: &apiclient.UpdateTaxAccountHeaders{IfMatch: revisionIfMatch(revision)}, Body: &in})
	if err != nil {
		return api.TaxAccount{}, taxMutationFailure(mutationRequestError(response, err))
	}
	receipt, err := decodeTaxReceipt(ctx, response, false)
	if err != nil {
		return api.TaxAccount{}, taxMutationFailure(err)
	}
	return validateTaxMutationResponse(&receipt.Items[0], response, id, revision, false)
}
func (c *Connection) RetireTaxAccount(ctx context.Context, id string, revision int64) (api.TaxAccount, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.TaxAccount{}, errors.New("invalid tax account ID or revision")
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).RetireTaxAccount(runtime.WithStreamingResponse(ctx), &apiclient.RetireTaxAccountRequestOptions{PathParams: &apiclient.RetireTaxAccountPath{AccountID: id}, Header: &apiclient.RetireTaxAccountHeaders{IfMatch: revisionIfMatch(revision)}})
	if err != nil {
		return api.TaxAccount{}, taxMutationFailure(mutationRequestError(response, err))
	}
	receipt, err := decodeTaxReceipt(ctx, response, false)
	if err != nil {
		return api.TaxAccount{}, taxMutationFailure(err)
	}
	return validateTaxMutationResponse(&receipt.Items[0], response, id, revision, true)
}
