package query

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ValidateTextOperand applies the QueryV1 filter rules to an expression operand.
func ValidateTextOperand(field, value string) error {
	var valid bool
	switch field {
	case "path":
		valid = validVirtualPath(value)
	case "mime":
		valid = validConcreteMIME(value)
	case "extension":
		valid = extensionPattern.MatchString(value)
	case "media_family":
		valid = validMediaFamily(value)
	default:
		return fmt.Errorf("unsupported text field %q", field)
	}
	if !utf8.ValidString(value) || !valid {
		return fmt.Errorf("invalid %s operand", field)
	}
	return nil
}

// ParseSizeOperand reads a decimal operand within the QueryV1 size bounds.
func ParseSizeOperand(value string) (int64, error) {
	if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, errors.New("size operand must be a decimal nonnegative safe integer")
	}
	bound, err := strconv.ParseInt(value, 10, 64)
	if err != nil || !validSizeBound(bound) {
		return 0, errors.New("size operand must be a decimal nonnegative safe integer")
	}
	return bound, nil
}

func validSizeBound(value int64) bool { return value >= 0 && value <= maxSafeInteger }
