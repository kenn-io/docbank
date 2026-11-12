package taxrecord

import (
	"errors"
	"strings"
	"testing"
)

func TestTaxEventValidation(t *testing.T) {
	amount := int64(1200)
	tests := []struct {
		name, recordKind string
		value            EventValues
		valid            bool
	}{
		{"filing state", "filing", EventValues{Kind: "filed", SetsState: "filed", OccurredOn: "2024-02-29"}, true},
		{"wrong state family", "audit", EventValues{Kind: "filed", SetsState: "filed", OccurredOn: "2024-02-29"}, false},
		{"assessment amount", "filing", EventValues{Kind: "assessed", SetsState: "assessed", OccurredOn: "2024-02-29", AmountMinor: &amount, Currency: "CAD"}, true},
		{"note with money", "filing", EventValues{Kind: "note", OccurredOn: "2024-02-29", AmountMinor: &amount, Currency: "CAD"}, false},
		{"currency without money", "filing", EventValues{Kind: "note", OccurredOn: "2024-02-29", Currency: "CAD"}, false},
		{"cash deferred", "filing", EventValues{Kind: "payment", OccurredOn: "2024-02-29"}, false},
		{"invalid date", "filing", EventValues{Kind: "note", OccurredOn: "2023-02-29"}, false},
		{"oversize reference", "filing", EventValues{Kind: "note", OccurredOn: "2024-02-29", Reference: strings.Repeat("x", 257)}, false},
		{"oversize note", "filing", EventValues{Kind: "note", OccurredOn: "2024-02-29", Note: strings.Repeat("x", 8193)}, false},
		{"noncanonical note", "filing", EventValues{Kind: "note", OccurredOn: "2024-02-29", Note: "a\r\nb"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEvent(tt.recordKind, tt.value)
			if (err == nil) != tt.valid || (err != nil && !errors.Is(err, ErrInvalid)) {
				t.Fatalf("ValidateEvent() = %v, valid = %v", err, tt.valid)
			}
		})
	}
}

func TestTaxStateIgnoresEventClocks(t *testing.T) {
	events := []EventFact{
		{Kind: "filed", SetsState: "filed", OccurredOn: "2025-01-01", RecordRevision: 2},
		{Kind: "assessed", SetsState: "assessed", OccurredOn: "2023-01-01", RecordRevision: 8},
		{Kind: "ready", SetsState: "ready", OccurredOn: "2026-01-01", RecordRevision: 5},
	}
	for _, tt := range []struct {
		void8, void5, void2 bool
		want                string
	}{
		{want: "assessed"}, {void8: true, want: "ready"}, {void8: true, void5: true, void2: true, want: "expected"},
	} {
		events[0].Voided, events[1].Voided, events[2].Voided = tt.void2, tt.void8, tt.void5
		got, err := StateFromEvents("filing", events)
		if err != nil || got != tt.want {
			t.Fatalf("StateFromEvents() = %q, %v; want %q", got, err, tt.want)
		}
	}
}

func TestTaxEventNormalizationAndBoundaries(t *testing.T) {
	value := EventValues{Kind: "note", OccurredOn: "2024-02-29", Reference: strings.Repeat("x", 256), Note: strings.Repeat("a", 8190) + "\r\n"}
	got, err := NormalizeEvent("filing", value)
	if err != nil || len(got.Note) != 8191 {
		t.Fatalf("NormalizeEvent = %#v, %v", got, err)
	}
	for _, value := range []EventValues{
		{Kind: "note", OccurredOn: "0000-01-01"},
		{Kind: "note", OccurredOn: "2024-02-29", Reference: "\xff"},
		{Kind: "note", OccurredOn: "2024-02-29", Reference: "bad\x00value"},
		{Kind: "refund", OccurredOn: "2024-02-29"},
	} {
		if err := ValidateEvent("filing", value); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateEvent returned %v", err)
		}
	}
	if got, err := NormalizeVoidReason("  "); !errors.Is(err, ErrInvalid) || got != "" {
		t.Fatalf("blank reason = %q, %v", got, err)
	}
}

func TestTaxEventAssessmentMoney(t *testing.T) {
	for _, tc := range []struct {
		currency, decimal string
		minor             int64
		valid             bool
	}{
		{"JPY", "123", 123, true}, {"BHD", "1.234", 1234, true}, {"JPY", "1.2", 0, false}, {"USD", "1e3", 0, false}, {"USD", "99999999999999999999999999999999", 0, false},
	} {
		got, err := ParseMoney(tc.decimal, tc.currency)
		if (err == nil) != tc.valid {
			t.Errorf("%s %s: %v", tc.currency, tc.decimal, err)
			continue
		}
		if err == nil {
			if got != tc.minor {
				t.Errorf("%s: got %d", tc.currency, got)
			}
			if err := ValidateEvent("filing", EventValues{Kind: "assessed", SetsState: "assessed", OccurredOn: "2026-01-01", Currency: tc.currency, AmountMinor: &got}); err != nil {
				t.Errorf("valid amount: %v", err)
			}
		}
	}
}
