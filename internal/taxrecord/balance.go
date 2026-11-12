package taxrecord

import "math"

// Balance is derived from a record and its surviving cash events.
type Balance struct {
	OutstandingMinor    *int64
	Currency            string
	OtherCurrencyEvents int64
}

// CalculateBalance excludes opposite-kind and foreign-currency cash from the sum.
func CalculateBalance(record RecordValues, events []EventFact) (Balance, error) {
	result := Balance{Currency: record.Currency}
	var sum int64
	for _, e := range events {
		if e.Voided || (e.Kind != "payment" && e.Kind != "refund") {
			continue
		}
		selected := record.Currency != "" && e.Currency == record.Currency && ((record.Direction == "payable" && e.Kind == "payment") || (record.Direction == "refundable" && e.Kind == "refund"))
		if !selected {
			result.OtherCurrencyEvents++
			continue
		}
		if sum > math.MaxInt64-*e.AmountMinor {
			return Balance{}, invalid("amount", "invalid cash sum")
		}
		sum += *e.AmountMinor
	}
	if record.Direction == "nil" {
		result.OutstandingMinor = new(int64(0))
	} else if record.Direction != "unknown" && record.AmountMinor != nil {
		result.OutstandingMinor = new(*record.AmountMinor - sum)
	}
	return result, nil
}
