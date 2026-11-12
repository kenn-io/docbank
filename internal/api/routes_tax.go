package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/taxrecord"
)

func registerTaxRoutes(api huma.API, d Deps, g *gate) { registerTaxAccountRoutes(api, d, g) }

// taxValidationError discards request values, parser messages, and unknown names.
func taxValidationError(_ huma.Context, status int, _ string, errs ...error) huma.StatusError {
	code := "validation"
	if status >= http.StatusInternalServerError {
		code = "internal"
	}
	out := NewError(status, code, "invalid tax request")
	known := map[string]bool{"body.filer_party_id": true, "body.authority_party_id": true, "body.country": true, "body.subdivision": true, "body.tax_kind": true, "body.identifier": true, "body.cadence": true, "body.period_note": true, "body.due_note": true, "body.lead_days": true, "body.currency": true, "body.effective_start": true, "body.effective_end": true, "body.note": true, "query.filer": true, "query.country": true, "query.taxkind": true, "query.include_retired": true, "query.limit": true, "query.offset": true, "query.reveal": true, "path.account_id": true, "header.If-Match": true}
	for _, err := range errs {
		if detail, ok := errors.AsType[*huma.ErrorDetail](err); ok && known[detail.Location] {
			out.Errors = append(out.Errors, detail.Location+": invalid value")
		}
	}
	return out
}
func taxRouteError(err error) error {
	if err == nil {
		return nil
	}
	var wire *Error
	if !errors.As(err, &wire) {
		wire, _ = FromStoreError(err).(*Error)
	}
	if wire == nil {
		return NewError(http.StatusInternalServerError, "internal", "tax request failed")
	}
	out := NewError(wire.Status, wire.Code, "tax request failed")
	if field, ok := errors.AsType[*taxrecord.FieldError](err); ok {
		out.Detail = field.Error()
	}
	return out
}
func isTaxOperation(ctx huma.Context) bool {
	return ctx.Operation() != nil && strings.HasPrefix(ctx.Operation().Path, "/api/v1/tax/")
}
