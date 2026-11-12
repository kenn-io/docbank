package taxrecord

import (
	"errors"
	"math"
	"testing"
)

func TestTaxMoneyExponents(t *testing.T) {
	for _, tc := range []struct {
		code, want string
		minor      int64
	}{{"USD", "1234.00", 123400}, {"JPY", "1234", 1234}, {"BHD", "1.234", 1234}, {"USD", "92233720368547758.07", math.MaxInt64}} {
		got, err := FormatMoney(tc.minor, tc.code)
		if err != nil || got != tc.want {
			t.Errorf("FormatMoney(%d, %q) = %q, %v", tc.minor, tc.code, got, err)
		}
		parsed, err := ParseMoney(got, tc.code)
		if err != nil || parsed != tc.minor {
			t.Errorf("roundtrip = %d, %v", parsed, err)
		}
	}

	for _, tc := range []struct {
		text, code string
		want       int64
	}{{"1234.5", "USD", 123450}, {"1.234", "KWD", 1234}, {"1234", "JPY", 1234}, {"9223372036854775807", "JPY", math.MaxInt64}} {
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
	for _, code := range []string{"???", "uſ"} {
		_, err := ParseMoney("1", code)
		var field *FieldError
		if !errors.As(err, &field) || field.Field != "currency" {
			t.Fatalf("currency: %v", err)
		}
	}
}

func TestTaxMoneyOverflowAfterPadding(t *testing.T) {
	if got, err := ParseMoney("92233720368547758.07", "USD"); err != nil || got != math.MaxInt64 {
		t.Fatalf("maximum: %d %v", got, err)
	}
	if _, err := ParseMoney("92233720368547759", "USD"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overflow: %v", err)
	}
}
