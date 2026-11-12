package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/taxrecord"
)

func registerTaxRoutes(api huma.API, d Deps, g *gate) {
	registerTaxAccountRoutes(api, d, g)
	registerTaxRecordRoutes(api, d, g)
}

// Each field records the Huma locations that may be reported for it. The
// checked client uses the same names and reasons to filter untrusted responses.
var taxDiagnosticFields = []struct{ name, locations string }{
	{"filer_party_id", "b"}, {"authority_party_id", "b"}, {"country", "bq"},
	{"subdivision", "b"}, {"tax_kind", "b"}, {"identifier", "b"}, {"cadence", "b"},
	{"period_note", "b"}, {"due_note", "b"}, {"lead_days", "b"}, {"currency", "b"},
	{"effective_start", "b"}, {"effective_end", "b"}, {"note", "b"}, {"filer", "q"},
	{"taxkind", "q"}, {"include_retired", "q"}, {packageLimitParameter, "q"}, {"offset", "q"},
	{"reveal", "q"}, {"account_id", "bp"}, {"If-Match", "h"},
	{"title", "b"}, {"kind", "bq"}, {"form_code", "b"}, {"tax_year", "b"},
	{"period_start", "b"}, {"period_end", "b"}, {"due_date", "b"},
	{"direction", "b"}, {"amount", "b"}, {"state", "q"}, {"year", "q"}, {"record_id", "p"},
}
var taxDiagnosticPairs = map[string]string{
	"invalid value": "filer", "required": "filer_party_id",
	"invalid length or text": "country subdivision currency tax_kind identifier period_note due_note note",
	"control character": "tax_kind identifier period_note due_note note",
	"invalid country": "country", "invalid subdivision": "subdivision", "invalid currency": "currency",
	"invalid date": "effective_start effective_end", "precedes effective_start": "effective_end",
	"not canonical": "tax_kind", "unknown cadence": "cadence", "out of range": "lead_days",
	"party not found": "filer_party_id authority_party_id", "party is retired": "filer_party_id authority_party_id",
	"party is not own": "filer_party_id", "party is not an organization": "authority_party_id",
}

func taxSafeFieldReason(field, reason string) bool {
	return strings.Contains(" "+taxDiagnosticPairs[reason]+" ", " "+field+" ") && taxDiagnosticPairs[reason] != ""
}

// SafeTaxDiagnostic extracts only a fixed field and reason from untrusted text.
// It never returns any surrounding server-supplied text.
func SafeTaxDiagnostic(detail string) string {
	best, result := len(detail), ""
	for _, field := range taxDiagnosticFields {
		for reason := range taxDiagnosticPairs {
			for _, prefix := range []string{"", "body.", "query."} {
				if prefix == "" && !taxSafeFieldReason(field.name, reason) || prefix != "" && (reason != "invalid value" || !taxSafeLocation(prefix+field.name)) {
					continue
				}
				safe := prefix + field.name + ": " + reason
				if index := strings.Index(detail, safe); index >= 0 && index < best {
					best, result = index, safe
				}
			}
		}
	}
	return result
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
func taxRouteError(logger *slog.Logger, err error) error {
	if err == nil {
		return nil
	}
	var wire *Error
	if !errors.As(err, &wire) {
		wire, _ = errors.AsType[*Error](FromStoreError(err))
	}
	detail := "tax request failed"
	if wire.Status == http.StatusServiceUnavailable && wire.Code == "maintenance_busy" {
		logger.Warn("tax request rejected", "class", "maintenance_busy", "status", wire.Status)
		detail = "vault maintenance is running or queued; retry this mutation after it finishes"
	} else if wire.Status >= http.StatusInternalServerError {
		logger.Error("tax request failed", "class", "tax_5xx", "status", wire.Status)
	}
	out := NewError(wire.Status, wire.Code, detail)
	if field, ok := errors.AsType[*taxrecord.FieldError](err); ok && taxSafeFieldReason(field.Field, field.Reason) {
		out.Detail = field.Error()
	}
	return out
}
func isTaxOperation(ctx huma.Context) bool {
	return ctx.Operation() != nil && strings.HasPrefix(ctx.Operation().Path, "/api/v1/tax/")
}

// taxCountrySelector uppercases ASCII without folding non-ASCII input.
func taxCountrySelector(value string) string {
	country := []byte(value)
	for i, c := range country {
		if c >= 'a' && c <= 'z' {
			country[i] = c - ('a' - 'A')
		}
	}
	return string(country)
}
