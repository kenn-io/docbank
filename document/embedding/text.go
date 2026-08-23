package embedding

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

func validateUTF8Text(name, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s contains invalid UTF-8", name)
	}
	return nil
}

func validateIdentityText(name, value string) error {
	if err := validateUTF8Text(name, value); err != nil {
		return err
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s contains a control character", name)
	}
	return nil
}

func validateContextText(context DocumentContext) error {
	if err := validateUTF8Text("document filename", context.Filename); err != nil {
		return err
	}
	return validateUTF8Text("document title", context.Title)
}
