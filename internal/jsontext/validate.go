// Package jsontext validates the lossless text boundary that encoding/json
// deliberately does not enforce.
package jsontext

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"unicode/utf8"
)

// Validate rejects byte sequences and escaped UTF-16 surrogate units that
// encoding/json would silently replace with U+FFFD. It intentionally is not a
// complete JSON syntax validator; callers still pass accepted input to their
// normal decoder.
func Validate(raw []byte, subject string) error {
	if !utf8.Valid(raw) {
		return fmt.Errorf("%s is not valid UTF-8", subject)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '"' {
			continue
		}
		for i++; i < len(raw) && raw[i] != '"'; i++ {
			if raw[i] != '\\' {
				continue
			}
			if i+1 >= len(raw) {
				return fmt.Errorf("%s contains an incomplete escape", subject)
			}
			if raw[i+1] != 'u' {
				i++
				continue
			}
			unit, ok := parseUTF16Unit(raw[i+2:])
			if !ok {
				return fmt.Errorf("%s contains an invalid Unicode escape", subject)
			}
			i += 5
			switch {
			case unit >= 0xd800 && unit <= 0xdbff:
				next := i + 1
				if next+6 > len(raw) || raw[next] != '\\' || raw[next+1] != 'u' {
					return fmt.Errorf("%s contains an unpaired UTF-16 surrogate escape", subject)
				}
				low, valid := parseUTF16Unit(raw[next+2:])
				if !valid || low < 0xdc00 || low > 0xdfff {
					return fmt.Errorf("%s contains an unpaired UTF-16 surrogate escape", subject)
				}
				i = next + 5
			case unit >= 0xdc00 && unit <= 0xdfff:
				return fmt.Errorf("%s contains an unpaired UTF-16 surrogate escape", subject)
			}
		}
	}
	return nil
}

// ValidateValue rejects invalid UTF-8 in any string that encoding/json would
// traverse. It covers the ordinary maps, slices, pointers, and structs used by
// typed request clients; the depth guard also turns cycles into a bounded
// error before marshaling.
func ValidateValue(value any, subject string) error {
	return validateValue(reflect.ValueOf(value), subject, 0)
}

func validateValue(value reflect.Value, subject string, depth int) error {
	if !value.IsValid() {
		return nil
	}
	if depth > 64 {
		return errors.New(subject + " is too deeply nested")
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if value.IsNil() {
			return nil
		}
		return validateValue(value.Elem(), subject, depth+1)
	case reflect.String:
		text := value.String()
		if !utf8.ValidString(text) {
			return fmt.Errorf("%s text %s is not valid UTF-8", subject, strconv.QuoteToASCII(text))
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			if err := validateValue(iter.Key(), subject, depth+1); err != nil {
				return err
			}
			if err := validateValue(iter.Value(), subject, depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range value.Len() {
			if err := validateValue(value.Index(i), subject, depth+1); err != nil {
				return err
			}
		}
	case reflect.Struct:
		valueType := value.Type()
		for i := range value.NumField() {
			field := valueType.Field(i)
			if field.PkgPath != "" || field.Tag.Get("json") == "-" {
				continue
			}
			if err := validateValue(value.Field(i), subject, depth+1); err != nil {
				return err
			}
		}
	default:
		// Scalars and channels/functions carry no JSON string text. Unsupported
		// values remain json.Marshal's responsibility.
	}
	return nil
}

func parseUTF16Unit(raw []byte) (uint16, bool) {
	if len(raw) < 4 {
		return 0, false
	}
	var value uint16
	for _, b := range raw[:4] {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value |= uint16(b - '0')
		case b >= 'a' && b <= 'f':
			value |= uint16(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value |= uint16(b-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}
