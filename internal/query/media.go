package query

import (
	"strings"

	"go.kenn.io/docbank/document"
)

var (
	queryFamilyByMIME      map[string]string
	queryFamilyByExtension map[string]string
)

func init() {
	queryFamilyByMIME = make(map[string]string)
	queryFamilyByExtension = make(map[string]string)
	for _, metadata := range document.FormatMetadataCatalog() {
		queryFamilyByMIME[metadata.MediaType] = metadata.QueryFamily
		for _, extension := range metadata.Extensions {
			queryFamilyByExtension[extension] = metadata.QueryFamily
		}
	}
}

// ClassifyMedia returns the QueryV1 family derived only from declared metadata.
// A valid concrete MIME type takes precedence; filename is consulted only when
// the MIME type is missing or the generic application/octet-stream value.
func ClassifyMedia(mediaType, filename string) string {
	trimmed := trimASCIIWhitespace(mediaType)
	if trimmed != "" {
		essence, ok := parseConcreteMediaType(trimmed)
		if !ok {
			return "unknown"
		}
		if essence != "application/octet-stream" {
			if family, ok := queryFamilyByMIME[essence]; ok {
				return family
			}
			if strings.HasPrefix(essence, "image/") {
				return "image"
			}
			if strings.HasPrefix(essence, "audio/") || strings.HasPrefix(essence, "video/") {
				return "audio_video"
			}
			return "unknown"
		}
	}
	if family, ok := queryFamilyByExtension[filenameExtension(filename)]; ok {
		return family
	}
	return "unknown"
}

// parseConcreteMediaType accepts a concrete RFC 2045 type/subtype followed by
// zero or more syntactically valid token or quoted-string parameters. Parameter
// values do not affect classification.
func parseConcreteMediaType(value string) (string, bool) {
	semicolon := strings.IndexByte(value, ';')
	essenceEnd := len(value)
	if semicolon >= 0 {
		essenceEnd = semicolon
	}
	essence, ok := lowerASCII(trimASCIIWhitespace(value[:essenceEnd]))
	if !ok {
		return "", false
	}
	if !validConcreteMIME(essence) {
		return "", false
	}
	if semicolon < 0 {
		return essence, true
	}

	names := make(map[string]struct{})
	for index := semicolon; index < len(value); {
		if value[index] != ';' {
			return "", false
		}
		index++
		index = skipMIMEWhitespace(value, index)
		nameStart := index
		for index < len(value) && validMIMEToken(value[index:index+1], true) {
			index++
		}
		name := strings.ToLower(value[nameStart:index])
		if name == "" {
			return "", false
		}
		if _, duplicate := names[name]; duplicate {
			return "", false
		}
		names[name] = struct{}{}
		index = skipMIMEWhitespace(value, index)
		if index >= len(value) || value[index] != '=' {
			return "", false
		}
		index++
		index = skipMIMEWhitespace(value, index)
		if index >= len(value) {
			return "", false
		}
		if value[index] == '"' {
			var ok bool
			index, ok = scanMIMEQuotedString(value, index+1)
			if !ok {
				return "", false
			}
		} else {
			valueStart := index
			for index < len(value) && validMIMEToken(value[index:index+1], true) {
				index++
			}
			if index == valueStart {
				return "", false
			}
		}
		index = skipMIMEWhitespace(value, index)
		if index < len(value) && value[index] != ';' {
			return "", false
		}
	}
	return essence, true
}

func skipMIMEWhitespace(value string, index int) int {
	for index < len(value) && (value[index] == ' ' || value[index] == '\t') {
		index++
	}
	return index
}

func scanMIMEQuotedString(value string, index int) (int, bool) {
	for index < len(value) {
		character := value[index]
		index++
		if character == '"' {
			return index, true
		}
		if character == '\\' {
			if index >= len(value) || invalidMIMEQuotedByte(value[index]) {
				return 0, false
			}
			index++
		} else if invalidMIMEQuotedByte(character) {
			return 0, false
		}
	}
	return 0, false
}

func invalidMIMEQuotedByte(character byte) bool {
	return character < 0x20 && character != '\t' || character == 0x7f
}

func filenameExtension(filename string) string {
	baseStart := strings.LastIndexAny(filename, `/\\`) + 1
	base := filename[baseStart:]
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 || dot == len(base)-1 {
		return ""
	}
	extension, ok := lowerASCII(base[dot+1:])
	if !ok {
		return ""
	}
	return extension
}

func trimASCIIWhitespace(value string) string {
	return strings.Trim(value, " \t\n\v\f\r")
}

func lowerASCII(value string) (string, bool) {
	result := []byte(value)
	for index, character := range result {
		if character >= 0x80 {
			return "", false
		}
		if character >= 'A' && character <= 'Z' {
			result[index] = character + ('a' - 'A')
		}
	}
	return string(result), true
}
