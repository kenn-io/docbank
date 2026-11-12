package taxrecord

import (
	"errors"
	"strings"
	"testing"
)

func TestTaxCountrySubdivisionCurrency(t *testing.T) {
	base := AccountValues{FilerPartyID: "filer", Country: "ZZ", TaxKind: "example", Cadence: "annual", Currency: "USD"}
	for _, tc := range []struct {
		country, subdivision, currency string
		valid                          bool
	}{
		{"ZZ", "ZZ-A1", "USD", true}, {"EU", "", "USD", false}, {"UN", "", "USD", false}, {"QO", "", "USD", false}, {"XA", "", "USD", false}, {"ZZ", "AA-A", "USD", false}, {"ZZ", "", "QQQ", false},
	} {
		v := base
		v.Country, v.Subdivision, v.Currency = tc.country, tc.subdivision, tc.currency
		_, err := NormalizeAccount(v)
		if (err == nil) != tc.valid {
			t.Errorf("%s %s %s: %v", tc.country, tc.subdivision, tc.currency, err)
		}
	}
}

func TestTaxTextBoundsAndCanonicalForms(t *testing.T) {
	v := AccountValues{FilerPartyID: "filer", Country: "zz", TaxKind: "  Cafe\u0301  ", Cadence: "annual", Currency: "usd"}
	got, err := NormalizeAccount(v)
	if err != nil || got.TaxKind != "Café" || got.Country != "ZZ" || got.Currency != "USD" {
		t.Fatalf("normalization: %+v, %v", got, err)
	}
	if err := ValidateAccount(v); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retained noncanonical input: %v", err)
	}
	v.TaxKind = strings.Repeat("x", 65)
	if _, err := NormalizeAccount(v); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized label: %v", err)
	}
}

func TestTaxInitialStateAndVerification(t *testing.T) {
	for _, kind := range []string{"filing", "payment", "audit", "review", "penalty", "notice", "other"} {
		state, err := InitialState(kind)
		if err != nil || state != map[bool]string{true: "opened", false: "expected"}[kind == "audit" || kind == "review"] {
			t.Errorf("%s: %s %v", kind, state, err)
		}
	}
	if _, err := InitialState("unknown"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestTaxDateAndRawLocationErrorsNameFields(t *testing.T) {
	base := AccountValues{FilerPartyID: "filer", Country: "ZZ", TaxKind: "example", Cadence: "annual", Currency: "USD"}
	for _, tc := range []struct {
		name   string
		change func(*AccountValues)
	}{
		{"country", func(v *AccountValues) { v.Country = strings.Repeat("z", 100) }},
		{"subdivision", func(v *AccountValues) { v.Subdivision = strings.Repeat("z", 100) }},
		{"currency", func(v *AccountValues) { v.Currency = strings.Repeat("u", 100) }},
		{"effective_start", func(v *AccountValues) { v.EffectiveStart = new("2026-02-30") }},
		{"effective_end", func(v *AccountValues) { v.EffectiveStart = new("2026-02-01"); v.EffectiveEnd = new("2026-01-01") }},
	} {
		v := base
		tc.change(&v)
		_, err := NormalizeAccount(v)
		var field *FieldError
		if !errors.As(err, &field) || field.Field != tc.name {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	r := RecordValues{FilerPartyID: "filer", Country: "ZZ", TaxKind: "example", Kind: "filing", Title: "Example", State: "expected", Direction: "unknown", Verification: "unverified", PeriodEnd: new("invalid")}
	_, err := NormalizeRecord(r)
	var field *FieldError
	if !errors.As(err, &field) || field.Field != "period_end" {
		t.Errorf("period_end: %v", err)
	}
}

func TestTaxDatesYearLeadDaysVocabulary(t *testing.T) {
	base := RecordValues{FilerPartyID: "filer", Country: "ZZ", TaxKind: "example", Kind: "filing", Title: "Example", State: "expected", Direction: "unknown", Verification: "unverified"}
	for _, tc := range []struct {
		name   string
		change func(*RecordValues)
	}{
		{"non-leap", func(v *RecordValues) { v.PeriodEnd = new("2025-02-29") }},
		{"reversed", func(v *RecordValues) { v.PeriodStart = new("2026-12-31"); v.PeriodEnd = new("2026-01-01") }},
		{"year", func(v *RecordValues) { v.TaxYear = new(int64(10000)) }},
		{"lead", func(v *RecordValues) { v.LeadDays = new(int64(367)) }},
		{"direction", func(v *RecordValues) { v.Direction = "refundable" }},
		{"amount", func(v *RecordValues) { v.AmountMinor = new(int64(0)) }},
		{"state", func(v *RecordValues) { v.State = "filed" }},
		{"verification", func(v *RecordValues) { v.Verification = "verified" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := base
			tc.change(&v)
			if err := ValidateRecord(v); err == nil {
				t.Fatal("accepted invalid record")
			}
		})
	}
	base.PeriodEnd = new("2024-02-29")
	base.Direction = "nil"
	base.AmountMinor = new(int64(0))
	base.Currency = "JPY"
	if err := ValidateRecord(base); err != nil {
		t.Fatal(err)
	}
}

func TestTaxTextPinnedBoundaries(t *testing.T) {
	base := AccountValues{FilerPartyID: "filer", Country: "ZZ", TaxKind: "example", Cadence: "annual", Currency: "USD"}
	base.Identifier = strings.Repeat("x", 256)
	base.Note = strings.Repeat("x", 8189) + "\r\ny"
	got, err := NormalizeAccount(base)
	if err != nil || got.Note != strings.Repeat("x", 8189)+"\ny" {
		t.Fatalf("boundary normalization: %v", err)
	}
	base.Identifier += "x"
	if _, err := NormalizeAccount(base); !errors.Is(err, ErrInvalid) {
		t.Fatalf("identifier bound: %v", err)
	}
	base.Identifier = "x"
	base.Note = strings.Repeat("x", 8193)
	if _, err := NormalizeAccount(base); !errors.Is(err, ErrInvalid) {
		t.Fatalf("note bound: %v", err)
	}
	base.Note = "bad\x01"
	if _, err := NormalizeAccount(base); !errors.Is(err, ErrInvalid) {
		t.Fatalf("control: %v", err)
	}
}
