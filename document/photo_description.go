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

const (
	// MaxPhotoTextBytes bounds a caption or the visible text of one description.
	MaxPhotoTextBytes = 16 << 10
	// MaxPhotoTagBytes bounds one tag name.
	MaxPhotoTagBytes = 255
)

// ValidatePhotoDescription bounds every field before storing provider output.
func ValidatePhotoDescription(value PhotoDescription) error {
	for _, text := range []string{value.Caption, value.VisibleText} {
		if len(text) > MaxPhotoTextBytes || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			return errors.New("photo description text is invalid")
		}
	}
	if len(value.Tags) > 64 {
		return errors.New("photo description has too many tags")
	}
	for _, tag := range value.Tags {
		if tag == "" || tag != strings.TrimSpace(tag) || len(tag) > MaxPhotoTagBytes || !utf8.ValidString(tag) || strings.ContainsRune(tag, 0) {
			return errors.New("photo description tag is invalid")
		}
	}
	return nil
}
