package document

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const maxSafeEmailFilenameBytes = 240

var reservedEmailFilenames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
	"com¹": true, "com²": true, "com³": true,
	"lpt¹": true, "lpt²": true, "lpt³": true,
}

func SafeEmailFilename(decoded, partPath string) (string, error) {
	if !utf8.ValidString(decoded) {
		return "", errors.New("email filename is not valid UTF-8")
	}
	if err := ValidateEmailPartPath(partPath); err != nil {
		return "", err
	}
	value := norm.NFC.String(decoded)
	var b strings.Builder
	for _, r := range value {
		switch {
		case unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	value = strings.TrimSpace(b.String())
	value = strings.TrimRight(value, ". ")
	fallback := "part-" + strings.ReplaceAll(partPath, ".", "-")
	if value == "" {
		value = fallback
	}
	value = truncateEmailFilename(value, maxSafeEmailFilenameBytes)
	if value == "" {
		value = truncateEmailFilename(fallback, maxSafeEmailFilenameBytes)
	}
	base := value
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	base = strings.TrimRight(base, " ")
	if reservedEmailFilenames[strings.ToLower(base)] {
		value = "_" + truncateEmailFilename(value, maxSafeEmailFilenameBytes-1)
	}
	return value, nil
}

func truncateEmailFilename(value string, limit int) string {
	if len(value) > limit {
		end := limit
		for end > 0 && !utf8.RuneStart(value[end]) {
			end--
		}
		value = value[:end]
	}
	return strings.TrimRight(value, ". ")
}
