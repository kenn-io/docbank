package document

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// PhotoDescription is one indivisible proposal from a vision provider.
type PhotoDescription struct {
	Caption     string   `json:"caption"`
	Tags        []string `json:"tags"`
	VisibleText string   `json:"visible_text"`
}

// ValidatePhotoDescription bounds every field before storing provider output.
func ValidatePhotoDescription(value PhotoDescription) error {
	for _, text := range []string{value.Caption, value.VisibleText} {
		if len(text) > 16<<10 || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			return errors.New("photo description text is invalid")
		}
	}
	if len(value.Tags) > 64 {
		return errors.New("photo description has too many tags")
	}
	seen := make(map[string]bool, len(value.Tags))
	for _, tag := range value.Tags {
		if tag == "" || tag != strings.TrimSpace(tag) || len(tag) > 255 || !utf8.ValidString(tag) || strings.ContainsRune(tag, 0) || seen[strings.ToLower(tag)] {
			return errors.New("photo description tag is invalid or duplicated")
		}
		seen[strings.ToLower(tag)] = true
	}
	return nil
}
