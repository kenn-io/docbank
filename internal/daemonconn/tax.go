package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
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
func validTaxAccount(a api.TaxAccount) bool {
	return validUUIDv4(a.AccountID) && validUUIDv4(a.FilerPartyID) && (a.AuthorityPartyID == nil || validUUIDv4(*a.AuthorityPartyID)) && a.Country != "" && a.TaxKind != "" && a.Cadence != "" && a.Currency != "" && a.Revision > 0 && a.CreatedAt != "" && a.UpdatedAt != ""
}
func validateTaxAccountResponse(a *api.TaxAccount, response *http.Response, id string) (api.TaxAccount, error) {
	if a == nil || response == nil || !validTaxAccount(*a) || (id != "" && a.AccountID != id) || response.Header.Get("ETag") != revisionIfMatch(a.Revision) {
		return api.TaxAccount{}, &responseDecodeError{err: errors.New("invalid tax account response")}
	}
	return *a, nil
}
func (c *Connection) TaxAccounts(ctx context.Context, filer, country, taxkind string, includeRetired bool, limit, offset int) (api.TaxAccountPage, error) {
	if limit < 1 || limit > 1000 || offset < 0 {
		return api.TaxAccountPage{}, errors.New("invalid tax account page")
	}
	queryLimit, queryOffset := int64(limit), int64(offset)
	page, err := c.API().ListTaxAccounts(ctx, &apiclient.ListTaxAccountsRequestOptions{Query: &apiclient.ListTaxAccountsQuery{Filer: &filer, Country: &country, Taxkind: &taxkind, IncludeRetired: &includeRetired, Limit: &queryLimit, Offset: &queryOffset}})
	if err != nil {
		return api.TaxAccountPage{}, safeTaxError(err)
	}
	if page == nil || page.Items == nil || page.Limit != limit || page.Offset != offset || len(page.Items) > limit {
		return api.TaxAccountPage{}, &responseDecodeError{err: errors.New("invalid tax account page")}
	}
	for _, a := range page.Items {
		if !validTaxAccount(a) {
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
		return api.TaxAccount{}, taxMutationFailure(mutationRequestError(response, err))
	}
	result, err := validateTaxAccountResponse(a, response, "")
	return result, taxMutationFailure(err)
}
func (c *Connection) UpdateTaxAccount(ctx context.Context, id string, revision int64, in api.PatchTaxAccountRequest) (api.TaxAccount, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.TaxAccount{}, errors.New("invalid tax account ID or revision")
	}
	var response *http.Response
	a, err := c.apiWithResponse(&response).UpdateTaxAccount(ctx, &apiclient.UpdateTaxAccountRequestOptions{PathParams: &apiclient.UpdateTaxAccountPath{AccountID: id}, Header: &apiclient.UpdateTaxAccountHeaders{IfMatch: revisionIfMatch(revision)}, Body: &in})
	if err != nil {
		return api.TaxAccount{}, taxMutationFailure(mutationRequestError(response, err))
	}
	result, err := validateTaxAccountResponse(a, response, id)
	return result, taxMutationFailure(err)
}
func (c *Connection) RetireTaxAccount(ctx context.Context, id string, revision int64) (api.TaxAccount, error) {
	if !validUUIDv4(id) || revision < 1 {
		return api.TaxAccount{}, errors.New("invalid tax account ID or revision")
	}
	var response *http.Response
	a, err := c.apiWithResponse(&response).RetireTaxAccount(ctx, &apiclient.RetireTaxAccountRequestOptions{PathParams: &apiclient.RetireTaxAccountPath{AccountID: id}, Header: &apiclient.RetireTaxAccountHeaders{IfMatch: revisionIfMatch(revision)}})
	if err != nil {
		return api.TaxAccount{}, taxMutationFailure(mutationRequestError(response, err))
	}
	result, err := validateTaxAccountResponse(a, response, id)
	return result, taxMutationFailure(err)
}
