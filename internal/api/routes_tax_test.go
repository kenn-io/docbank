package api

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/taxrecord"
)

func TestTaxFieldErrorRedactsUnknownDetails(t *testing.T) {
	t.Parallel()
	for _, field := range []*taxrecord.FieldError{
		{Field: "private-synthetic-value", Reason: "invalid value"},
		{Field: "cadence", Reason: "private-synthetic-value"},
		{Field: "filer", Reason: "unknown cadence"},
	} {
		var problem *Error
		require.ErrorAs(t, taxRouteError(slog.Default(), field), &problem)
		assert.NotContains(t, problem.Detail, "private-synthetic-value")
	}

}

func TestTaxSafeDiagnosticOrderAndServerFailure(t *testing.T) {
	t.Parallel()
	const secret = "private-synthetic-value"
	assert.Equal(t, "cadence: unknown cadence", SafeTaxDiagnostic("filer: unknown cadence; cadence: unknown cadence "+secret))
	assert.Empty(t, SafeTaxDiagnostic("query.cadence: invalid value; body.filer: invalid value"))
	assert.Empty(t, SafeTaxDiagnostic("body.cadence: unknown cadence"))
	assert.Equal(t, "body.note: invalid value", SafeTaxDiagnostic("body.note: invalid value "+secret))
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	var problem *Error
	require.ErrorAs(t, taxRouteError(logger, errors.New(secret)), &problem)
	assert.Equal(t, http.StatusInternalServerError, problem.Status)
	assert.Contains(t, logs.String(), "tax_5xx")
	assert.NotContains(t, logs.String(), secret)
	assert.NotContains(t, problem.Error(), secret)
}

// Mounted cases wire the server.go middleware order around the base-path boundary.
func TestTaxMalformedPathMuxRedirectIsRedacted(t *testing.T) {
	t.Parallel()
	const secret = "private-synthetic-value"
	for _, base := range []string{"", "/archive", "/tools/archive"} {
		for _, raw := range []string{
			"/api/v1/tax/accounts/" + secret,
			"/api/v1//tax/accounts/" + secret,
			"/api/v1/tax/accounts/..%2F..%2F" + secret,
			"/api/v1/tax/accounts/../../" + secret,
			"/%61pi/v1/%74ax/accounts/../../" + secret,
			"/%61pi/v1/%74ax/accounts/%2e%2e%2F" + secret,
			"/api/v1/other/" + secret,
		} {
			t.Run(base+raw, func(t *testing.T) {
				t.Parallel()
				var logs bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&logs, nil))
				mux := http.NewServeMux()
				mux.HandleFunc("/api/v1/tax/accounts/{id}", func(http.ResponseWriter, *http.Request) { panic(secret) })
				mux.HandleFunc("/api/v1/other/{id}", func(http.ResponseWriter, *http.Request) { panic("ordinary panic") })
				handler := logMiddleware(recoverMiddleware(webBasePathMiddleware(mux, base), logger, base), logger, base)
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, base+raw, nil))
				if strings.HasPrefix(raw, "/api/v1/other/") {
					assert.Contains(t, logs.String(), base+raw)
					assert.Contains(t, logs.String(), "ordinary panic")
					return
				}
				assert.NotContains(t, logs.String(), secret)
				assert.Contains(t, logs.String(), "/api/v1/tax/{redacted}")
				if strings.Contains(raw, "/../") {
					logs.Reset()
					panicHandler := recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(secret) }), logger, base)
					panicHandler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, base+raw, nil))
					assert.Contains(t, logs.String(), "panic in tax handler")
					assert.NotContains(t, logs.String(), secret)
				}
				if strings.Contains(raw, "//tax/") || strings.Contains(raw, "/../") {
					assert.Equal(t, http.StatusTemporaryRedirect, recorder.Code)
				} else {
					assert.Equal(t, http.StatusInternalServerError, recorder.Code)
					assert.Contains(t, logs.String(), "panic in tax handler")
				}
			})
		}
	}
}

func TestTaxMaintenanceErrorIsSafe(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	var problem *Error
	require.ErrorAs(t, taxRouteError(logger, NewError(http.StatusServiceUnavailable, "maintenance_busy", "private-synthetic-value")), &problem)
	assert.Contains(t, problem.Detail, "retry")
	assert.Contains(t, logs.String(), "WARN")
	assert.Contains(t, logs.String(), "maintenance_busy")
	assert.NotContains(t, logs.String(), "private-synthetic-value")
	assert.NotContains(t, problem.Detail, "private-synthetic-value")
}

func TestTaxFormatterPreservesMediaValidation(t *testing.T) {
	t.Parallel()
	const secret = "private-synthetic-media-url"
	ctx := humago.NewContext(&huma.Operation{Path: "/api/v1/media/occurrences"}, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/media/occurrences", nil), httptest.NewRecorder())
	problem := huma.NewErrorWithContext(ctx, http.StatusUnprocessableEntity, "validation", &huma.ErrorDetail{Location: "body.canonical_url", Value: secret, Message: "invalid"})
	var result *Error
	require.ErrorAs(t, problem, &result)
	assert.Equal(t, []string{"invalid media reference body"}, result.Errors)
	assert.NotContains(t, result.Error(), secret)
}

type failingTaxResponse struct{ *httptest.ResponseRecorder }

func (w failingTaxResponse) Write([]byte) (int, error) {
	return 0, errors.New("synthetic write failure")
}

func TestTaxOversizeWriteFailureOmitsPath(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	const sentinel = "private-synthetic-value"
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tax/accounts/..%2F..%2F"+sentinel, nil)
	request.ContentLength = 10
	ctx := humago.NewContext(&huma.Operation{Path: "/api/v1/tax/accounts/{account_id}", MaxBodyBytes: 1}, request, failingTaxResponse{httptest.NewRecorder()})
	called := false
	rejectDeclaredOversizeBodies(NewOfflineServer().API(), logger)(ctx, func(huma.Context) { called = true })
	assert.False(t, called)
	assert.Contains(t, logs.String(), "write oversize body rejection")
	assert.Contains(t, logs.String(), "/api/v1/tax/{redacted}")
	assert.NotContains(t, logs.String(), sentinel)
}

// These inputs reach account admission; record-only reasons are owned by T3b.
func TestTaxAccountAdmissionDiagnosticsRemainSafe(t *testing.T) {
	t.Parallel()
	base := taxrecord.AccountValues{FilerPartyID: "filer", Country: "ZZ", TaxKind: "example", Cadence: "annual", Currency: "USD"}
	cases := []struct {
		field, reason string
		change        func(*taxrecord.AccountValues)
	}{
		{"filer_party_id", "required", func(v *taxrecord.AccountValues) { v.FilerPartyID = "" }},
		{"tax_kind", "not canonical", func(v *taxrecord.AccountValues) { v.TaxKind = " example " }},
		{"identifier", "control character", func(v *taxrecord.AccountValues) { v.Identifier = "\x00" }},
		{"identifier", "invalid length or text", func(v *taxrecord.AccountValues) { v.Identifier = strings.Repeat("x", 257) }},
		{"tax_kind", "invalid length or text", func(v *taxrecord.AccountValues) { v.TaxKind = "" }},
		{"country", "invalid length or text", func(v *taxrecord.AccountValues) { v.Country = "ZZZ" }},
		{"period_note", "control character", func(v *taxrecord.AccountValues) { v.PeriodNote = "\x00" }},
		{"note", "invalid length or text", func(v *taxrecord.AccountValues) { v.Note = string(make([]byte, 8193)) }},
		{"country", "invalid country", func(v *taxrecord.AccountValues) { v.Country = "AA" }},
		{"subdivision", "invalid subdivision", func(v *taxrecord.AccountValues) { v.Subdivision = "AA-A" }},
		{"currency", "invalid currency", func(v *taxrecord.AccountValues) { v.Currency = "???" }},
		{"cadence", "unknown cadence", func(v *taxrecord.AccountValues) { v.Cadence = "future" }},
		{"lead_days", "out of range", func(v *taxrecord.AccountValues) { v.LeadDays = 367 }},
		{"effective_start", "invalid date", func(v *taxrecord.AccountValues) { v.EffectiveStart = new("2025-02-30") }},
		{"effective_end", "precedes effective_start", func(v *taxrecord.AccountValues) {
			v.EffectiveStart = new("2025-02-01")
			v.EffectiveEnd = new("2025-01-01")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.field+"/"+tc.reason, func(t *testing.T) {
			t.Parallel()
			v := base
			tc.change(&v)
			var field *taxrecord.FieldError
			require.ErrorAs(t, taxrecord.ValidateAccount(v), &field)
			assert.Equal(t, tc.field, field.Field)
			assert.Equal(t, tc.reason, field.Reason)
			var problem *Error
			require.ErrorAs(t, taxRouteError(slog.Default(), field), &problem)
			assert.Equal(t, field.Error(), problem.Detail)
		})
	}
	// These are the store's account party-admission outcomes.
	for _, reason := range []string{"party not found", "party is retired", "party is not own", "party is not an organization"} {
		name := "filer_party_id"
		if reason == "party is not an organization" {
			name = "authority_party_id"
		}
		field := &taxrecord.FieldError{Field: name, Reason: reason}
		var problem *Error
		require.ErrorAs(t, taxRouteError(slog.Default(), field), &problem)
		assert.Equal(t, field.Error(), problem.Detail)
	}
}
