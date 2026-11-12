package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/taxrecord"
)

func registerTaxRoutes(api huma.API, d Deps, g *gate) { registerTaxAccountRoutes(api, d, g) }

// Each field records the Huma locations that may be reported for it. The
// checked client uses the same names and reasons to filter untrusted responses.
var taxDiagnosticFields = []struct{ name, locations string }{
	{"filer_party_id", "b"}, {"authority_party_id", "b"}, {"country", "bq"},
	{"subdivision", "b"}, {"tax_kind", "b"}, {"identifier", "b"}, {"cadence", "b"},
	{"period_note", "b"}, {"due_note", "b"}, {"lead_days", "b"}, {"currency", "b"},
	{"effective_start", "b"}, {"effective_end", "b"}, {"note", "b"}, {"filer", "q"},
	{"taxkind", "q"}, {"include_retired", "q"}, {packageLimitParameter, "q"}, {"offset", "q"},
	{"reveal", "q"}, {"account_id", "p"}, {"If-Match", "h"},
}
var taxDiagnosticReasons = []string{"invalid value", "invalid length or text", "control character", "invalid country", "invalid subdivision", "invalid currency", "invalid date", "precedes effective_start", "not canonical", "required", "unknown cadence", "out of range", "party not found", "party is retired", "party is not own", "party is not an organization"}

func taxSafeFieldReason(field, reason string) bool {
	if !slices.Contains(taxDiagnosticReasons, reason) {
		return false
	}
	for _, known := range taxDiagnosticFields {
		if known.name == field {
			return true
		}
	}
	return false
}

// SafeTaxDiagnostic extracts only a fixed field and reason from untrusted text.
// It never returns any surrounding server-supplied text.
func SafeTaxDiagnostic(detail string) string {
	for _, field := range taxDiagnosticFields {
		for _, reason := range taxDiagnosticReasons {
			for _, prefix := range []string{"", "body.", "query.", "path.", "header."} {
				safe := prefix + field.name + ": " + reason
				if strings.Contains(detail, safe) {
					return safe
				}
			}
		}
	}
	return ""
}

func taxSafeLocation(location string) bool {
	prefix, name, ok := strings.Cut(location, ".")
	if !ok {
		return false
	}
	code := map[string]string{"body": "b", openAPIQueryLocation: "q", openAPIPathLocation: "p", mailboxHeaderLocation: "h"}[prefix]
	if code == "" {
		return false
	}
	for _, field := range taxDiagnosticFields {
		if field.name == name && strings.Contains(field.locations, code) {
			return true
		}
	}
	return false
}

// taxValidationError discards request values, parser messages, and unknown names.
func taxValidationError(_ huma.Context, status int, _ string, errs ...error) huma.StatusError {
	code := "validation"
	if status >= http.StatusInternalServerError {
		code = "internal"
	}
	out := NewError(status, code, "invalid tax request")
	for _, err := range errs {
		if detail, ok := errors.AsType[*huma.ErrorDetail](err); ok && taxSafeLocation(detail.Location) {
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
		wire, _ = errors.AsType[*Error](FromStoreError(err))
	}
	if wire == nil {
		return NewError(http.StatusInternalServerError, "internal", "tax request failed")
	}
	out := NewError(wire.Status, wire.Code, "tax request failed")
	if field, ok := errors.AsType[*taxrecord.FieldError](err); ok && taxSafeFieldReason(field.Field, field.Reason) {
		out.Detail = field.Error()
	}
	return out
}
func isTaxOperation(ctx huma.Context) bool {
	return ctx.Operation() != nil && strings.HasPrefix(ctx.Operation().Path, "/api/v1/tax/")
}
