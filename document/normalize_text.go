package document

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

func safeCodeLanguage(language string) string {
	if language == "" || len(language) > 64 {
		return ""
	}
	for _, character := range language {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("+#-_.", character) {
			continue
		}
		return ""
	}
	return language
}

func isHTMLBlockElement(tag string) bool {
	switch tag {
	case "address", "article", "aside", "blockquote", "body", "caption", "center", "colgroup", "dd", "details", "dialog", "dir", "div", "dl", "dt", "fieldset", "figcaption", "figure", "footer", "form", "header", "hgroup", "hr", "html", "main", "menu", "nav", "ol", "p", "search", "section", "summary", "table", "tbody", "tfoot", "thead", "ul":
		return true
	default:
		return false
	}
}

func stripUnsafeControls(value string) string {
	return strings.Map(func(character rune) rune {
		switch character {
		case '\n', '\t':
			return character
		case '\f', '\v', '\u0085', '\u2028', '\u2029':
			return '\n'
		}
		if unicode.IsSpace(character) {
			return ' '
		}
		if unicode.IsControl(character) || character == headingSentinelStart || character == headingSentinelEnd {
			return -1
		}
		return character
	}, strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n"))
}

func safeStoredLink(value string, maxChars int) string {
	if utf8.RuneCountInString(value) > maxChars ||
		strings.ContainsRune(value, headingSentinelStart) || strings.ContainsRune(value, headingSentinelEnd) {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return ""
	}
	return parsed.String()
}

func truncateUTF8Bytes(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut], true
}

func truncateRunes(value string, limit int) (string, bool) {
	if limit < 0 {
		limit = 0
	}
	if utf8.RuneCountInString(value) <= limit {
		return value, false
	}
	for byteOffset := range value {
		if limit == 0 {
			return value[:byteOffset], true
		}
		limit--
	}
	return value, false
}
