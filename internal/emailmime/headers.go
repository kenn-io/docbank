package emailmime

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.kenn.io/docbank/document"
)

type parsedHeaderBlock struct {
	raw    []byte
	fields []parsedHeader
}
type parsedHeader struct {
	index  int
	offset int64
	length int64
	name   string
	value  string
	valid  bool
}

type policyLimitError struct {
	code      document.EmailDiagnosticCode
	operation document.EmailOperation
	path      string
	limit     int64
	observed  int64
}

func (e *policyLimitError) Error() string { return "email MIME declared limit exceeded" }

func readHeaderBlock(ctx context.Context, reader *bufio.Reader, maxBytes int64, maxFields int, aggregateBytes *int64, aggregateByteLimit int64, aggregateFields *int, aggregateFieldLimit int) (parsedHeaderBlock, error) {
	raw := make([]byte, 0, min(int(maxBytes), 4096))
	fieldsReserved := 0
	atLineStart := true
	previousValid := false
	tentativeLine := false
	reserveField := func() error {
		observed := fieldsReserved + 1
		if observed > maxFields {
			return &policyLimitError{code: document.EmailDiagnosticHeaderFieldsLimit, operation: document.EmailOperationHeaders, limit: int64(maxFields), observed: int64(observed)}
		}
		if *aggregateFields+1 > aggregateFieldLimit {
			return &policyLimitError{code: document.EmailDiagnosticHeaderTotalFieldsLimit, operation: document.EmailOperationHeaders, limit: int64(aggregateFieldLimit), observed: int64(*aggregateFields + 1)}
		}
		fieldsReserved++
		*aggregateFields++
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return parsedHeaderBlock{}, fmt.Errorf("read MIME header byte: %w", err)
		}
		value, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if tentativeLine && !atLineStart {
					if reserveErr := reserveField(); reserveErr != nil {
						return parsedHeaderBlock{}, reserveErr
					}
				}
				fields := parseHeaderFields(raw)
				return parsedHeaderBlock{raw: raw, fields: fields}, nil
			}
			return parsedHeaderBlock{}, fmt.Errorf("read MIME header byte: %w", err)
		}
		if atLineStart {
			tentativeLine = value == '\r' || (previousValid && (value == ' ' || value == '\t'))
			if value != '\n' && !tentativeLine {
				if reserveErr := reserveField(); reserveErr != nil {
					return parsedHeaderBlock{}, reserveErr
				}
			}
		} else if tentativeLine && len(raw) > 0 && raw[len(raw)-1] == '\r' && value != '\n' {
			if reserveErr := reserveField(); reserveErr != nil {
				return parsedHeaderBlock{}, reserveErr
			}
			tentativeLine = false
		}
		observed := int64(len(raw)) + 1
		if observed > maxBytes {
			return parsedHeaderBlock{}, &policyLimitError{code: document.EmailDiagnosticHeaderBytesLimit, operation: document.EmailOperationHeaders, limit: maxBytes, observed: observed}
		}
		if *aggregateBytes+1 > aggregateByteLimit {
			return parsedHeaderBlock{}, &policyLimitError{code: document.EmailDiagnosticHeaderTotalBytesLimit, operation: document.EmailOperationHeaders, limit: aggregateByteLimit, observed: *aggregateBytes + 1}
		}
		raw = append(raw, value)
		*aggregateBytes++
		if value == '\n' {
			start := bytes.LastIndexByte(raw[:len(raw)-1], '\n') + 1
			line := raw[start:]
			if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
				fields := parseHeaderFields(raw)
				return parsedHeaderBlock{raw: raw, fields: fields}, nil
			}
			trimmed := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
			if len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t') && previousValid && !bytes.Contains(trimmed, []byte{'\r'}) {
				previousValid = true
			} else {
				colon := bytes.IndexByte(trimmed, ':')
				previousValid = !bytes.Contains(trimmed, []byte{'\r'}) && colon > 0 && validHeaderName(trimmed[:colon])
			}
			atLineStart = true
			tentativeLine = false
		} else {
			atLineStart = false
		}
	}
}

func parseHeaderFields(raw []byte) []parsedHeader {
	fields := make([]parsedHeader, 0, 32)
	start := 0
	for start < len(raw) {
		end := bytes.IndexByte(raw[start:], '\n')
		lineComplete := end >= 0
		if end < 0 {
			end = len(raw) - start
		} else {
			end++
		}
		line := raw[start : start+end]
		trimmed := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(trimmed) == 0 {
			break
		}
		if (trimmed[0] == ' ' || trimmed[0] == '\t') && lineComplete && !bytes.Contains(trimmed, []byte{'\r'}) && len(fields) > 0 && fields[len(fields)-1].valid {
			field := &fields[len(fields)-1]
			field.length += int64(len(line))
			start += end
			continue
		}
		field := parsedHeader{index: len(fields), offset: int64(start), length: int64(len(line))}
		if colon := bytes.IndexByte(trimmed, ':'); lineComplete && !bytes.Contains(trimmed, []byte{'\r'}) && colon > 0 && validHeaderName(trimmed[:colon]) {
			field.valid = true
			field.name = strings.ToLower(string(trimmed[:colon]))
		}
		fields = append(fields, field)
		start += end
	}
	for index := range fields {
		if fields[index].valid {
			span := raw[fields[index].offset : fields[index].offset+fields[index].length]
			fields[index].value = unfoldHeaderValue(span)
		}
	}
	return fields
}

func unfoldHeaderValue(span []byte) string {
	var builder strings.Builder
	builder.Grow(len(span))
	first := true
	for len(span) > 0 {
		end := bytes.IndexByte(span, '\n')
		if end < 0 {
			end = len(span)
		} else {
			end++
		}
		line := bytes.TrimSuffix(bytes.TrimSuffix(span[:end], []byte("\n")), []byte("\r"))
		if first {
			colon := bytes.IndexByte(line, ':')
			line = line[colon+1:]
			first = false
		} else {
			builder.WriteByte(' ')
		}
		builder.WriteString(strings.TrimSpace(string(line)))
		span = span[end:]
	}
	return builder.String()
}

func validHeaderName(value []byte) bool {
	for _, c := range value {
		if c < 33 || c > 126 || strings.ContainsRune("()<>@,;:\\\"/[]?={} ", rune(c)) {
			return false
		}
	}
	return len(value) > 0
}

func headerValues(block parsedHeaderBlock, name string) []parsedHeader {
	var out []parsedHeader
	for _, field := range block.fields {
		if field.valid && field.name == name {
			out = append(out, field)
		}
	}
	return out
}

func emailHeaders(block parsedHeaderBlock) []document.EmailHeaderV1 {
	out := make([]document.EmailHeaderV1, 0, len(block.fields))
	for _, field := range block.fields {
		var name *string
		state := document.EmailHeaderMalformed
		if field.valid {
			value := field.name
			name = &value
			state = document.EmailHeaderValid
		}
		out = append(out, document.EmailHeaderV1{Index: field.index, Offset: field.offset, Length: field.length, Name: name, State: state})
	}
	return out
}
