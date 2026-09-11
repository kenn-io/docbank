package emailmime

import (
	"errors"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"golang.org/x/net/html/charset"
)

func (d *decoder) interpretMessage(path string, block parsedHeaderBlock) document.EmailMessageV1 {
	message := document.EmailMessageV1{Path: path, Fields: emptyEmailFields(), Alternatives: []document.EmailAlternativeV1{}, RelatedGroups: []document.EmailRelatedGroupV1{}, Diagnostics: []document.EmailDiagnosticV1{}}
	for _, definition := range []struct {
		name    string
		address bool
		target  *[]document.EmailDecodedFieldV1
	}{
		{"subject", false, &message.Fields.Subject}, {"from", true, &message.Fields.From}, {"to", true, &message.Fields.To}, {"cc", true, &message.Fields.Cc}, {"bcc", true, &message.Fields.Bcc}, {"reply-to", true, &message.Fields.ReplyTo}, {"message-id", false, &message.Fields.MessageID}, {"in-reply-to", false, &message.Fields.InReplyTo}, {"references", false, &message.Fields.References},
	} {
		for _, field := range headerValues(block, definition.name) {
			entry := document.EmailDecodedFieldV1{HeaderIndex: field.index, State: document.EmailInterpretationDecoded}
			remaining := d.limits.HeaderDisplayBytes - d.headerDisplayBytes
			decoded, err := decodeHeaderBounded(field.value, remaining)
			encodedWordErr := err
			if err != nil && !errors.Is(err, errDisplayBudget) && utf8.ValidString(field.value) {
				decoded, err = boundedUTF8(field.value, remaining)
			}
			if errors.Is(err, errDisplayBudget) {
				entry.State = document.EmailInterpretationUnsupported
				message.Diagnostics = append(message.Diagnostics, d.diagnosticAt(document.EmailDiagnosticHeaderDisplayLimit, document.EmailOperationHeaderDisplay, path, field.index, "decoded header display exceeds its aggregate byte limit")...)
				*definition.target = append(*definition.target, entry)
				continue
			}
			validDisplay := utf8.ValidString(decoded)
			if !validDisplay {
				entry.State = document.EmailInterpretationInvalid
				message.Diagnostics = append(message.Diagnostics, d.diagnosticAt(document.EmailDiagnosticEncodedWordInvalid, document.EmailOperationHeaderDisplay, path, field.index, "encoded header display is invalid UTF-8")...)
				*definition.target = append(*definition.target, entry)
				continue
			}
			cost := int64(len(decoded))
			var addresses *[]document.EmailAddressV1
			if definition.address {
				parsed, addressCost, addressErr := projectAddressesBounded(field.value, remaining-cost)
				if addressErr != nil {
					if errors.Is(addressErr, errDisplayBudget) {
						entry.State = document.EmailInterpretationUnsupported
						message.Diagnostics = append(message.Diagnostics, d.diagnosticAt(document.EmailDiagnosticHeaderDisplayLimit, document.EmailOperationHeaderDisplay, path, field.index, "decoded address display exceeds its aggregate byte limit")...)
						*definition.target = append(*definition.target, entry)
						continue
					}
					entry.State = document.EmailInterpretationInvalid
					message.Diagnostics = append(message.Diagnostics, d.diagnosticAt(document.EmailDiagnosticInvalidHeader, document.EmailOperationHeaders, path, field.index, "address header is invalid")...)
				} else {
					cost += addressCost
					addresses = &parsed
				}
			}
			if d.headerDisplayBytes+cost > d.limits.HeaderDisplayBytes {
				entry.State = document.EmailInterpretationUnsupported
				entry.Text = nil
				entry.Addresses = nil
				message.Diagnostics = append(message.Diagnostics, d.diagnosticAt(document.EmailDiagnosticHeaderDisplayLimit, document.EmailOperationHeaderDisplay, path, field.index, "decoded header display exceeds its aggregate byte limit")...)
			} else {
				d.headerDisplayBytes += cost
				text := decoded
				entry.Text = &text
				entry.Addresses = addresses
				if encodedWordErr != nil {
					entry.State = document.EmailInterpretationInvalid
					message.Diagnostics = append(message.Diagnostics, d.diagnosticAt(document.EmailDiagnosticEncodedWordInvalid, document.EmailOperationHeaderDisplay, path, field.index, "encoded header word is invalid")...)
				}
			}
			*definition.target = append(*definition.target, entry)
		}
	}
	message.Date = d.interpretDate(path, headerValues(block, "date"))
	return message
}

func emptyEmailFields() document.EmailFieldsV1 {
	return document.EmailFieldsV1{Subject: []document.EmailDecodedFieldV1{}, From: []document.EmailDecodedFieldV1{}, To: []document.EmailDecodedFieldV1{}, Cc: []document.EmailDecodedFieldV1{}, Bcc: []document.EmailDecodedFieldV1{}, ReplyTo: []document.EmailDecodedFieldV1{}, MessageID: []document.EmailDecodedFieldV1{}, InReplyTo: []document.EmailDecodedFieldV1{}, References: []document.EmailDecodedFieldV1{}}
}

const filenameDisplayLimitState document.EmailInterpretationState = "_display_limit"

func (d *decoder) interpretFilename(path string, block parsedHeaderBlock) (document.EmailFilenameV1, []document.EmailDiagnosticV1) {
	fields := append(headerValues(block, "content-disposition"), headerValues(block, "content-type")...)
	if len(fields) == 0 {
		return document.EmailFilenameV1{Fields: []int{}, State: document.EmailInterpretationMissing}, nil
	}
	indexes := make([]int, 0, len(fields))
	var decoded string
	state := document.EmailInterpretationMissing
	for _, field := range fields {
		params := parseRawMIMEParameters(field.value)
		fieldPresent := false
		for _, key := range []string{"filename", "name"} {
			value, present, parameterState := decodeFilenameParameter(params, key, d.limits.HeaderDisplayBytes-d.headerDisplayBytes)
			if !present {
				continue
			}
			fieldPresent = true
			if state == document.EmailInterpretationMissing {
				decoded, state = value, parameterState
			}
		}
		if fieldPresent {
			indexes = append(indexes, field.index)
		}
	}
	if state == document.EmailInterpretationMissing {
		return document.EmailFilenameV1{Fields: []int{}, State: document.EmailInterpretationMissing}, nil
	}
	slices.Sort(indexes)
	indexes = slices.Compact(indexes)
	if state == filenameDisplayLimitState {
		return document.EmailFilenameV1{Fields: indexes, State: document.EmailInterpretationUnsupported}, d.diagnostic(document.EmailDiagnosticHeaderDisplayLimit, document.EmailOperationFilename, path, nil, "decoded filename exceeds the aggregate header display limit")
	}
	if state == document.EmailInterpretationUnsupported {
		return document.EmailFilenameV1{Fields: indexes, State: state}, d.diagnostic(document.EmailDiagnosticFilenameUnsupported, document.EmailOperationFilename, path, nil, "filename charset is unsupported")
	}
	if state == document.EmailInterpretationInvalid {
		return document.EmailFilenameV1{Fields: indexes, State: document.EmailInterpretationInvalid}, d.diagnostic(document.EmailDiagnosticFilenameInvalid, document.EmailOperationFilename, path, nil, "filename encoding is invalid")
	}
	if !utf8.ValidString(decoded) {
		return document.EmailFilenameV1{Fields: indexes, State: document.EmailInterpretationInvalid}, d.diagnostic(document.EmailDiagnosticFilenameInvalid, document.EmailOperationFilename, path, nil, "filename encoding is invalid")
	}
	if d.headerDisplayBytes+int64(len(decoded)) > d.limits.HeaderDisplayBytes {
		return document.EmailFilenameV1{Fields: indexes, State: document.EmailInterpretationUnsupported}, d.diagnostic(document.EmailDiagnosticHeaderDisplayLimit, document.EmailOperationFilename, path, nil, "decoded filename exceeds the aggregate header display limit")
	}
	d.headerDisplayBytes += int64(len(decoded))
	safe, err := document.SafeEmailFilename(decoded, path)
	if err != nil {
		return document.EmailFilenameV1{Fields: indexes, State: document.EmailInterpretationInvalid}, d.diagnostic(document.EmailDiagnosticFilenameInvalid, document.EmailOperationFilename, path, nil, "filename cannot be normalized")
	}
	return document.EmailFilenameV1{Fields: indexes, Decoded: &decoded, State: document.EmailInterpretationDecoded, SafeName: safe}, nil
}

type rawMIMEParameter struct {
	name  string
	value string
	valid bool
}

func parseRawMIMEParameters(value string) []rawMIMEParameter {
	semicolon := strings.IndexByte(value, ';')
	if semicolon < 0 {
		return nil
	}
	value = value[semicolon+1:]
	var result []rawMIMEParameter
	for len(value) > 0 {
		end, quoted, escaped := len(value), false, false
		for index, char := range []byte(value) {
			if escaped {
				escaped = false
				continue
			}
			if quoted && char == '\\' {
				escaped = true
				continue
			}
			if char == '"' {
				quoted = !quoted
			} else if char == ';' && !quoted {
				end = index
				break
			}
		}
		segment := strings.TrimSpace(value[:end])
		if end == len(value) {
			value = ""
		} else {
			value = value[end+1:]
		}
		name, parameterValue, ok := strings.Cut(segment, "=")
		if !ok {
			continue
		}
		name = strings.ToLower(strings.TrimSpace(name))
		parameterValue, valid := decodeRawMIMEParameterValue(strings.TrimSpace(parameterValue))
		result = append(result, rawMIMEParameter{name: name, value: parameterValue, valid: valid})
	}
	return result
}

func decodeRawMIMEParameterValue(value string) (string, bool) {
	if value == "" || value[0] != '"' {
		return value, validMIMEParameterToken(value)
	}
	if len(value) < 2 || value[len(value)-1] != '"' {
		return "", false
	}
	inner := value[1 : len(value)-1]
	if !strings.ContainsRune(inner, '\\') {
		if strings.ContainsAny(inner, "\r\n\"") {
			return "", false
		}
		return inner, true
	}
	var decoded strings.Builder
	for index := 1; index < len(value)-1; index++ {
		if value[index] == '\r' || value[index] == '\n' || value[index] == '"' {
			return "", false
		}
		if value[index] == '\\' {
			index++
			if index >= len(value)-1 || value[index] == '\r' || value[index] == '\n' {
				return "", false
			}
		}
		decoded.WriteByte(value[index])
	}
	return decoded.String(), true
}

func validMIMEParameterToken(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range []byte(value) {
		if char < 33 || char > 126 || strings.ContainsRune("()<>@,;:\\\"/[]?=", rune(char)) {
			return false
		}
	}
	return true
}

func decodeFilenameParameter(params []rawMIMEParameter, base string, limit int64) (string, bool, document.EmailInterpretationState) {
	var direct *string
	extended := make(map[int]rawMIMEParameter)
	hasExtended := false
	invalidParameters := false
	present := false
	for _, param := range params {
		if param.name == base {
			present = true
			if direct != nil || !param.valid {
				invalidParameters = true
				continue
			}
			value := param.value
			direct = &value
			continue
		}
		if param.name == base+"*" {
			present = true
			if _, exists := extended[0]; exists || !param.valid {
				invalidParameters = true
			}
			extended[0] = param
			hasExtended = true
			continue
		}
		prefix := base + "*"
		if !strings.HasPrefix(param.name, prefix) {
			continue
		}
		present = true
		suffix := strings.TrimSuffix(param.name[len(prefix):], "*")
		index, err := strconv.Atoi(suffix)
		if err != nil || index < 0 {
			hasExtended = true
			invalidParameters = true
			continue
		}
		if _, exists := extended[index]; exists || !param.valid {
			invalidParameters = true
		}
		extended[index] = param
		hasExtended = true
	}
	if invalidParameters {
		return "", present, document.EmailInterpretationInvalid
	}
	if !hasExtended {
		if direct == nil {
			return "", false, document.EmailInterpretationMissing
		}
		if !utf8.ValidString(*direct) {
			return "", true, document.EmailInterpretationInvalid
		}
		decoded, err := decodeHeaderBounded(*direct, limit)
		if errors.Is(err, errDisplayBudget) {
			return "", true, filenameDisplayLimitState
		}
		if err != nil || !utf8.ValidString(decoded) {
			return "", true, document.EmailInterpretationInvalid
		}
		return decoded, true, document.EmailInterpretationDecoded
	}
	first, ok := extended[0]
	if !ok {
		return "", true, document.EmailInterpretationInvalid
	}
	charsetName := ""
	firstValue := first.value
	if strings.HasSuffix(first.name, "*") {
		var rest string
		charsetName, rest, ok = strings.Cut(first.value, "'")
		if !ok {
			return "", true, document.EmailInterpretationInvalid
		}
		_, firstValue, ok = strings.Cut(rest, "'")
		if !ok || charsetName == "" {
			return "", true, document.EmailInterpretationInvalid
		}
	}
	var input strings.Builder
	for index := range len(extended) {
		param, exists := extended[index]
		if !exists {
			return "", true, document.EmailInterpretationInvalid
		}
		value := param.value
		if index == 0 {
			value = firstValue
		}
		if strings.HasSuffix(param.name, "*") {
			decoded, err := url.PathUnescape(value)
			if err != nil {
				return "", true, document.EmailInterpretationInvalid
			}
			value = decoded
		}
		input.WriteString(value)
	}
	var reader io.Reader = strings.NewReader(input.String())
	if charsetName != "" {
		converted, err := charset.NewReaderLabel(charsetName, reader)
		if err != nil {
			return "", true, document.EmailInterpretationUnsupported
		}
		reader = converted
	}
	// Raw parameters fit in the bounded header; only charset expansion needs
	// a separate retained-output limit.
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return "", true, document.EmailInterpretationInvalid
	}
	if int64(len(data)) > limit {
		return "", true, filenameDisplayLimitState
	}
	decoded, err := boundedUTF8(string(data), limit)
	if errors.Is(err, errDisplayBudget) {
		return "", true, filenameDisplayLimitState
	}
	if err != nil {
		return "", true, document.EmailInterpretationInvalid
	}
	return decoded, true, document.EmailInterpretationDecoded
}

func (d *decoder) interpretContentID(path string, block parsedHeaderBlock) (document.EmailContentIDV1, []document.EmailDiagnosticV1) {
	fields := headerValues(block, "content-id")
	if len(fields) == 0 {
		return document.EmailContentIDV1{Fields: []int{}, State: document.EmailInterpretationMissing}, nil
	}
	indexes := headerIndexes(fields)
	if len(fields) > 1 {
		return document.EmailContentIDV1{Fields: indexes, State: document.EmailInterpretationInvalid}, d.diagnostic(document.EmailDiagnosticContentIDAmbiguous, document.EmailOperationCID, path, nil, "multiple Content-ID fields are ambiguous")
	}
	value := strings.TrimSpace(fields[0].value)
	if len(value) >= 2 && value[0] == '<' && value[len(value)-1] == '>' {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	if value == "" || !utf8.ValidString(value) {
		return document.EmailContentIDV1{Fields: indexes, State: document.EmailInterpretationInvalid}, d.diagnostic(document.EmailDiagnosticContentIDInvalid, document.EmailOperationCID, path, nil, "Content-ID is invalid")
	}
	if d.headerDisplayBytes+int64(len(value)) > d.limits.HeaderDisplayBytes {
		return document.EmailContentIDV1{Fields: indexes, State: document.EmailInterpretationUnsupported}, d.diagnostic(document.EmailDiagnosticHeaderDisplayLimit, document.EmailOperationCID, path, nil, "Content-ID exceeds the aggregate header display limit")
	}
	d.headerDisplayBytes += int64(len(value))
	return document.EmailContentIDV1{Fields: indexes, Value: &value, State: document.EmailInterpretationDecoded}, nil
}

func headerIndexes(fields []parsedHeader) []int {
	out := make([]int, len(fields))
	for i, field := range fields {
		out[i] = field.index
	}
	return out
}
