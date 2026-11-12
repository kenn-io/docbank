package taxrecord

import (
	"errors"
	"math"
	"testing"
)

func TestTaxMoneyExponentsAndDirection(t *testing.T) {
	for _, tc := range []struct {
		text, code string
		want       int64
	}{{"1234.50", "USD", 123450}, {"1.234", "KWD", 1234}, {"1234", "JPY", 1234}, {"9223372036854775807", "JPY", math.MaxInt64}} {
		got, err := ParseMoney(tc.text, tc.code)
		if err != nil || got != tc.want {
			t.Errorf("ParseMoney(%q, %q) = %d, %v; want %d", tc.text, tc.code, got, err, tc.want)
		}
	}
}

func TestTaxMoneyOverflowAndSyntax(t *testing.T) {
	for _, text := range []string{"9223372036854775808", "-1", "+1", "1.1", "1e3", "1,000", " 1", "123456789012345678901234567890123", "\xff"} {
		if _, err := ParseMoney(text, "JPY"); err == nil {
			t.Errorf("accepted invalid amount %q", text)
		}
	}
}

func TestTaxMoneyCurrencyErrorField(t *testing.T) {
	_, err := ParseMoney("1", "???")
	var field *FieldError
	if !errors.As(err, &field) || field.Field != "currency" {
		t.Fatalf("currency: %v", err)
	}
}
