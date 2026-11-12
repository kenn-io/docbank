package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

// taxClientError keeps transport and response classification without printing request values.
type taxClientError struct{ cause error }

func (e *taxClientError) Error() string {
	message := "tax request failed"
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
				var problem *problemError
				if errors.As(e.cause, &problem) {
					detail := problem.Error()
					fields := []string{"filer_party_id", "authority_party_id", "country", "subdivision", "tax_kind", "identifier", "cadence", "period_note", "due_note", "lead_days", "currency", "effective_start", "effective_end", "note", "filer", "taxkind", "include_retired", "limit", "offset", "reveal", "account_id", "If-Match"}
					reasons := []string{"invalid value", "invalid length or text", "control character", "invalid country", "invalid subdivision", "invalid currency", "invalid date", "precedes effective_start", "not canonical", "required", "unknown cadence", "out of range", "party not found", "party is retired", "party is not own", "party is not an organization"}
					for _, field := range fields {
						for _, reason := range reasons {
							for _, prefix := range []string{"", "body.", "query.", "path.", "header."} {
								safe := prefix + field + ": " + reason
								if strings.Contains(detail, safe) {
									message += "; " + safe
									return message
								}
							}
						}
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
func validateTaxAccountResponse(a *api.TaxAccount, response *http.Response, id string) (api.TaxAccount, error) {
	if a == nil || response == nil || !validUUIDv4(a.AccountID) || a.Revision < 1 || (id != "" && a.AccountID != id) || response.Header.Get("ETag") != revisionIfMatch(a.Revision) {
		return api.TaxAccount{}, &responseDecodeError{err: errors.New("invalid tax account response")}
	}
	return *a, nil
}
func (c *Connection) TaxAccounts(ctx context.Context, o api.TaxAccountListOptions) (api.TaxAccountPage, error) {
	if o.Limit < 1 || o.Limit > 1000 || o.Offset < 0 {
		return api.TaxAccountPage{}, errors.New("invalid tax account page")
	}
	limit, offset := int64(o.Limit), int64(o.Offset)
	page, err := c.API().ListTaxAccounts(ctx, &apiclient.ListTaxAccountsRequestOptions{Query: &apiclient.ListTaxAccountsQuery{Filer: &o.FilerPartyID, Country: &o.Country, Taxkind: &o.TaxKind, IncludeRetired: &o.IncludeRetired, Limit: &limit, Offset: &offset}})
	if err != nil {
		return api.TaxAccountPage{}, safeTaxError(err)
	}
	if page == nil || page.Items == nil || page.Limit != o.Limit || page.Offset != o.Offset || len(page.Items) > o.Limit {
		return api.TaxAccountPage{}, &responseDecodeError{err: errors.New("invalid tax account page")}
	}
	for _, a := range page.Items {
		if !validUUIDv4(a.AccountID) || a.Revision < 1 {
			return api.TaxAccountPage{}, &responseDecodeError{err: errors.New("invalid tax account page")}
		}
	}
	return *page, nil
}
func (c *Connection) TaxAccount(ctx context.Context, id string, reveal bool) (api.TaxAccount, error) {
	if !validUUIDv4(id) {
		return api.TaxAccount{}, errors.New("invalid tax account ID")
	}
	var response *http.Response
	a, err := c.apiWithResponse(&response).GetTaxAccount(ctx, &apiclient.GetTaxAccountRequestOptions{PathParams: &apiclient.GetTaxAccountPath{AccountID: id}, Query: &apiclient.GetTaxAccountQuery{Reveal: &reveal}})
	if err != nil {
		return api.TaxAccount{}, safeTaxError(err)
	}
	return validateTaxAccountResponse(a, response, id)
}
func (c *Connection) CreateTaxAccount(ctx context.Context, in api.CreateTaxAccountRequest) (api.TaxAccount, error) {
	var response *http.Response
	a, err := c.apiWithResponse(&response).CreateTaxAccount(ctx, &apiclient.CreateTaxAccountRequestOptions{Body: &in})
	if err != nil {
		return api.TaxAccount{}, safeTaxError(mutationRequestError(response, err))
	}
	return validateTaxAccountResponse(a, response, "")
}
func (c *Connection) UpdateTaxAccount(ctx context.Context, id string, revision int64, in api.PatchTaxAccountRequest) (api.TaxAccount, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.TaxAccount{}, errors.New("invalid tax account ID or revision")
	}
	var response *http.Response
	a, err := c.apiWithResponse(&response).UpdateTaxAccount(ctx, &apiclient.UpdateTaxAccountRequestOptions{PathParams: &apiclient.UpdateTaxAccountPath{AccountID: id}, Header: &apiclient.UpdateTaxAccountHeaders{IfMatch: revisionIfMatch(revision)}, Body: &in})
	if err != nil {
		return api.TaxAccount{}, safeTaxError(mutationRequestError(response, err))
	}
	return validateTaxAccountResponse(a, response, id)
}
func (c *Connection) RetireTaxAccount(ctx context.Context, id string, revision int64) (api.TaxAccount, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.TaxAccount{}, errors.New("invalid tax account ID or revision")
	}
	var response *http.Response
	a, err := c.apiWithResponse(&response).RetireTaxAccount(ctx, &apiclient.RetireTaxAccountRequestOptions{PathParams: &apiclient.RetireTaxAccountPath{AccountID: id}, Header: &apiclient.RetireTaxAccountHeaders{IfMatch: revisionIfMatch(revision)}})
	if err != nil {
		return api.TaxAccount{}, safeTaxError(mutationRequestError(response, err))
	}
	return validateTaxAccountResponse(a, response, id)
}
