package taxrecord

import (
	"math"
	"strconv"
	"strings"

	"golang.org/x/text/currency"
)

// ParseMoney converts an unsigned decimal amount to exact minor units.
func ParseMoney(text, code string) (int64, error) {
	if len(code) != 3 || code != strings.ToUpper(code) {
		return 0, invalid("currency", "invalid currency")
	}
	if len(text) == 0 || len(text) > 32 {
		return 0, invalid("amount", "invalid length")
	}
	unit, err := currency.ParseISO(code)
	if err != nil {
		return 0, invalid("currency", "invalid currency")
	}
	scale, _ := currency.Standard.Rounding(unit)
	parts := strings.Split(text, ".")
	if len(parts) > 2 || len(parts[0]) == 0 || (len(parts) == 2 && (len(parts[1]) == 0 || len(parts[1]) > scale)) {
		return 0, invalid("amount", "invalid precision")
	}
	var n int64
	for _, p := range parts {
		for _, c := range []byte(p) {
			if c < '0' || c > '9' || n > (math.MaxInt64-int64(c-'0'))/10 {
				return 0, invalid("amount", "invalid decimal or overflow")
			}
			n = n*10 + int64(c-'0')
		}
	}
	fractional := 0
	if len(parts) == 2 {
		fractional = len(parts[1])
	}
	for ; fractional < scale; fractional++ {
		if n > math.MaxInt64/10 {
			return 0, invalid("amount", "overflow")
		}
		n *= 10
	}
	return n, nil
}

// FormatMoney converts retained minor units to exact unsigned decimal text.
func FormatMoney(minor int64, code string) (string, error) {
	if minor < 0 {
		return "", invalid("amount", "invalid decimal or overflow")
	}
	unit, err := currency.ParseISO(code)
	if err != nil || len(code) != 3 || code != strings.ToUpper(code) {
		return "", invalid("currency", "invalid currency")
	}
	scale, _ := currency.Standard.Rounding(unit)
	digits := strconv.FormatInt(minor, 10)
	if scale == 0 {
		return digits, nil
	}
	if len(digits) <= scale {
		digits = strings.Repeat("0", scale+1-len(digits)) + digits
	}
	return digits[:len(digits)-scale] + "." + digits[len(digits)-scale:], nil
}
