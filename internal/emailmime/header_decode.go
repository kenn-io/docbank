package emailmime

import (
	"errors"
	"fmt"
	"mime"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
)

var errDisplayBudget = errors.New("email decoded display exceeds its byte budget")
var headerWordDecoder = mime.WordDecoder{CharsetReader: charset.NewReaderLabel}

// The raw header limit bounds parser allocations; the display limit applies
// to retained output. Callers validate UTF-8 before publishing decoded text.
func decodeHeaderBounded(header string, limit int64) (string, error) {
	if limit < 0 || int64(len(header)) > Recipe().Limits.HeaderBytes {
		return "", errDisplayBudget
	}
	decoded, err := headerWordDecoder.DecodeHeader(header)
	if err != nil {
		return "", fmt.Errorf("decode email header: %w", err)
	}
	if int64(len(decoded)) > limit {
		return "", errDisplayBudget
	}
	return decoded, nil
}

func boundedUTF8(value string, limit int64) (string, error) {
	if !utf8.ValidString(value) {
		return "", errors.New("decoded display is invalid UTF-8")
	}
	if int64(len(value)) > limit {
		return "", errDisplayBudget
	}
	return value, nil
}
