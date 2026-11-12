package taxrecord

import (
	"errors"
	"math"
	"testing"
)

func TestTaxCashEventValidation(t *testing.T) {
	for _, kind := range []string{"payment", "refund"} {
		for _, tc := range []struct {
			amount          *int64
			currency, state string
			valid           bool
		}{
			{new(int64(1)), "USD", "", true}, {nil, "USD", "", false},
			{new(int64(0)), "USD", "", false}, {new(int64(-1)), "USD", "", false},
			{new(int64(1)), "", "", false}, {new(int64(1)), "USD", "filed", false},
		} {
			err := ValidateEvent("filing", EventValues{Kind: kind, OccurredOn: "2026-01-01", AmountMinor: tc.amount, Currency: tc.currency, SetsState: tc.state})
			if (err == nil) != tc.valid {
				t.Errorf("%s: %#v: %v", kind, tc, err)
			}
		}
	}
	if err := ValidateEvent("filing", EventValues{Kind: "payment", OccurredOn: "2026-01-01", AmountMinor: new(int64(1)), Currency: "USD", Reference: "  "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank reference: %v", err)
	}
}

func TestTaxBalanceRules(t *testing.T) {
	record := RecordValues{Direction: "payable", AmountMinor: new(int64(123400)), Currency: "USD"}
	events := []EventFact{
		{EventValues: EventValues{Kind: "payment", Currency: "USD", AmountMinor: new(int64(10000))}},
		{EventValues: EventValues{Kind: "payment", Currency: "EUR", AmountMinor: new(int64(10))}},
		{EventValues: EventValues{Kind: "refund", Currency: "EUR", AmountMinor: new(int64(10))}},
		{EventValues: EventValues{Kind: "refund", Currency: "USD", AmountMinor: new(int64(10))}},
		{EventValues: EventValues{Kind: "assessed", Currency: "USD", AmountMinor: new(int64(10))}},
		{EventValues: EventValues{Kind: "payment", Currency: "USD", AmountMinor: new(int64(10))}, Voided: true},
	}
	got, err := CalculateBalance(record, events)
	if err != nil || got.OutstandingMinor == nil || *got.OutstandingMinor != 113400 || got.OtherCurrencyEvents != 3 {
		t.Fatalf("balance = %+v, %v", got, err)
	}
	record.Currency = ""
	got, err = CalculateBalance(record, events)
	if err != nil || got.OtherCurrencyEvents != 4 {
		t.Fatalf("empty currency: %+v, %v", got, err)
	}
	record.Currency, record.Direction = "USD", "refundable"
	got, err = CalculateBalance(record, events)
	if err != nil || *got.OutstandingMinor != 123390 || got.OtherCurrencyEvents != 3 {
		t.Fatalf("refund: %+v, %v", got, err)
	}
	record.Direction, record.AmountMinor = "nil", nil
	got, err = CalculateBalance(record, nil)
	if err != nil || got.OutstandingMinor == nil || *got.OutstandingMinor != 0 {
		t.Fatalf("nil: %+v, %v", got, err)
	}
	record.Direction = "unknown"
	got, err = CalculateBalance(record, nil)
	if err != nil || got.OutstandingMinor != nil {
		t.Fatalf("unknown: %+v, %v", got, err)
	}
	record.Direction, record.Currency, record.AmountMinor = "payable", "USD", new(int64(1))
	got, err = CalculateBalance(record, events[:1])
	if err != nil || *got.OutstandingMinor != -9999 {
		t.Fatalf("overpayment: %+v, %v", got, err)
	}
	events[0].AmountMinor = new(int64(math.MaxInt64))
	events[1].Currency, events[1].AmountMinor = "USD", new(int64(1))
	_, err = CalculateBalance(record, events[:2])
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("overflow: %v", err)
	}
}
