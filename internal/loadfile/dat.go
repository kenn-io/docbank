package loadfile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	MaxColumnsPerRow   = 512
	MaxFieldValueBytes = 64 << 10
	MaxRowsPerPage     = 4096

	maxDecodedRowBytes = 1 << 20
)

var ErrLoadfileLimit = errors.New("package_limit: bounded load-file page exceeded")

func ScanDAT(source io.Reader, profile Profile, emit func(Record) error) ([]Diagnostic, error) {
	if emit == nil {
		return nil, ErrInvalidProfile
	}
	if err := validateDATProfile(profile); err != nil {
		return nil, err
	}
	decoder, err := Decoder(profile.Encoding)
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(decoder(source), validationReadSize)
	return scanDecodedRows(func() ([]string, bool, error) {
		return readDATRow(reader, profile)
	}, profile, emit)
}

func ParseDAT(source io.Reader, profile Profile) ([]Record, []Diagnostic, error) {
	return collectRecords(func(emit func(Record) error) ([]Diagnostic, error) {
		return ScanDAT(source, profile, emit)
	})
}

func collectRecords(scan func(func(Record) error) ([]Diagnostic, error)) ([]Record, []Diagnostic, error) {
	rows := make([]Record, 0, 100)
	diagnostics, err := scan(func(row Record) error {
		if len(rows) == MaxRowsPerPage {
			return ErrLoadfileLimit
		}
		rows = append(rows, row)
		return nil
	})
	return rows, diagnostics, err
}

func scanDecodedRows(next func() ([]string, bool, error), profile Profile, emit func(Record) error) ([]Diagnostic, error) {
	columns := append([]string(nil), profile.Columns...)
	diagnostics := make([]Diagnostic, 0)
	sourceOrdinal := 0
	headerPending := profile.HeaderRow
	for {
		values, ok, err := next()
		if err != nil {
			return diagnostics, err
		}
		if !ok {
			return diagnostics, nil
		}
		sourceOrdinal++
		if err := validateRowValues(values, sourceOrdinal); err != nil {
			return diagnostics, err
		}
		if headerPending {
			if len(columns) > 0 && !slices.Equal(values, columns) {
				return diagnostics, fmt.Errorf("%w: header does not match declared columns", ErrMalformedInput)
			}
			columns = append(columns[:0], values...)
			headerPending = false
			continue
		}
		record, diagnostic := makeRecord(values, columns, sourceOrdinal)
		if diagnostic != nil {
			if err := appendDiagnosticBounded(&diagnostics, *diagnostic); err != nil {
				return diagnostics, err
			}
		}
		if err := emit(record); err != nil {
			return diagnostics, err
		}
	}
}

func makeRecord(values, columns []string, rowOrdinal int) (Record, *Diagnostic) {
	fields := make([]Field, len(values))
	for index, value := range values {
		column := ""
		if index < len(columns) {
			column = columns[index]
		}
		fields[index] = Field{Column: column, Ordinal: index, Raw: value}
	}
	record := Record{
		RowOrdinal:  rowOrdinal,
		ColumnOrder: append([]string(nil), columns...),
		Fields:      fields,
	}
	if len(values) == len(columns) {
		return record, nil
	}
	diagnostic := &Diagnostic{
		Code:       "column_count_mismatch",
		Severity:   diagnosticSeverityBlocking,
		RowOrdinal: rowOrdinal,
		Detail:     fmt.Sprintf("record has %d columns; profile declares %d", len(values), len(columns)),
	}
	return record, diagnostic
}

func validateRowValues(values []string, rowOrdinal int) error {
	if len(values) > MaxColumnsPerRow {
		return fmt.Errorf("%w: row %d exceeds %d columns", ErrMalformedInput, rowOrdinal, MaxColumnsPerRow)
	}
	totalBytes := 0
	for _, value := range values {
		if len(value) > MaxFieldValueBytes {
			return fmt.Errorf("%w: row %d field exceeds %d decoded bytes", ErrMalformedInput, rowOrdinal, MaxFieldValueBytes)
		}
		if len(value) > maxDecodedRowBytes-totalBytes {
			return fmt.Errorf("%w: row %d exceeds %d normalized bytes", ErrMalformedInput, rowOrdinal, maxDecodedRowBytes)
		}
		totalBytes += len(value)
	}
	return nil
}

type datState uint8

const (
	datFieldStart datState = iota
	datInQualified
	datInBare
)

func readDATRow(reader *bufio.Reader, profile Profile) ([]string, bool, error) {
	fields := make([]string, 0, min(len(profile.Columns), MaxColumnsPerRow))
	var field strings.Builder
	state := datFieldStart
	rowStarted := false
	normalizedBytes := 0

	appendRune := func(value rune) error {
		size := utf8.RuneLen(value)
		if size < 0 {
			return ErrMalformedInput
		}
		if size > MaxFieldValueBytes-field.Len() {
			return fmt.Errorf("%w: DAT field exceeds %d decoded bytes", ErrMalformedInput, MaxFieldValueBytes)
		}
		if size > maxDecodedRowBytes-normalizedBytes {
			return fmt.Errorf("%w: DAT row exceeds %d normalized bytes", ErrMalformedInput, maxDecodedRowBytes)
		}
		field.WriteRune(value)
		normalizedBytes += size
		return nil
	}
	finishField := func() error {
		if len(fields) == MaxColumnsPerRow {
			return fmt.Errorf("%w: DAT row exceeds %d columns", ErrMalformedInput, MaxColumnsPerRow)
		}
		fields = append(fields, field.String())
		field.Reset()
		return nil
	}

	for {
		value, _, err := reader.ReadRune()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, false, fmt.Errorf("read DAT input: %w", err)
			}
			switch state {
			case datInQualified:
				return nil, false, fmt.Errorf("%w: unterminated qualified DAT field", ErrMalformedInput)
			case datInBare:
				if err := finishField(); err != nil {
					return nil, false, err
				}
				return fields, true, nil
			case datFieldStart:
				if !rowStarted {
					return nil, false, nil
				}
				if err := finishField(); err != nil {
					return nil, false, err
				}
				return fields, true, nil
			}
		}

		switch state {
		case datFieldStart:
			rowStarted = true
			switch value {
			case profile.Qualifier:
				state = datInQualified
			case profile.Field:
				if err := finishField(); err != nil {
					return nil, false, err
				}
			case '\n':
				if err := finishField(); err != nil {
					return nil, false, err
				}
				return fields, true, nil
			case '\r':
				if profile.NewlineInField == '\r' {
					if err := appendRune('\n'); err != nil {
						return nil, false, err
					}
					state = datInBare
					continue
				}
				if err := consumeOptionalLF(reader); err != nil {
					return nil, false, fmt.Errorf("read DAT row terminator: %w", err)
				}
				if err := finishField(); err != nil {
					return nil, false, err
				}
				return fields, true, nil
			default:
				if value == profile.NewlineInField && profile.NewlineInField != 0 {
					value = '\n'
				}
				if err := appendRune(value); err != nil {
					return nil, false, err
				}
				state = datInBare
			}

		case datInBare:
			switch value {
			case profile.Field:
				if err := finishField(); err != nil {
					return nil, false, err
				}
				state = datFieldStart
			case profile.Qualifier:
				return nil, false, fmt.Errorf("%w: qualifier in bare DAT field", ErrMalformedInput)
			case '\n':
				if err := finishField(); err != nil {
					return nil, false, err
				}
				return fields, true, nil
			case '\r':
				if profile.NewlineInField == '\r' {
					if err := appendRune('\n'); err != nil {
						return nil, false, err
					}
					continue
				}
				if err := consumeOptionalLF(reader); err != nil {
					return nil, false, fmt.Errorf("read DAT row terminator: %w", err)
				}
				if err := finishField(); err != nil {
					return nil, false, err
				}
				return fields, true, nil
			default:
				if value == profile.NewlineInField && profile.NewlineInField != 0 {
					value = '\n'
				}
				if err := appendRune(value); err != nil {
					return nil, false, err
				}
			}

		case datInQualified:
			if value == profile.NewlineInField && profile.NewlineInField != 0 {
				if err := appendRune('\n'); err != nil {
					return nil, false, err
				}
				continue
			}
			if value != profile.Qualifier {
				if err := appendRune(value); err != nil {
					return nil, false, err
				}
				continue
			}

			next, _, nextErr := reader.ReadRune()
			if errors.Is(nextErr, io.EOF) {
				if err := finishField(); err != nil {
					return nil, false, err
				}
				return fields, true, nil
			}
			if nextErr != nil {
				return nil, false, fmt.Errorf("read DAT qualifier lookahead: %w", nextErr)
			}
			switch next {
			case profile.Qualifier:
				if err := appendRune(profile.Qualifier); err != nil {
					return nil, false, err
				}
			case profile.Field:
				if err := finishField(); err != nil {
					return nil, false, err
				}
				state = datFieldStart
			case '\n':
				if err := finishField(); err != nil {
					return nil, false, err
				}
				return fields, true, nil
			case '\r':
				if err := consumeOptionalLF(reader); err != nil {
					return nil, false, fmt.Errorf("read DAT row terminator: %w", err)
				}
				if err := finishField(); err != nil {
					return nil, false, err
				}
				return fields, true, nil
			default:
				return nil, false, fmt.Errorf("%w: qualifier must be doubled or close a DAT field", ErrMalformedInput)
			}
		}
	}
}

func validateDATProfile(profile Profile) error {
	if profile.Field == 0 || profile.Qualifier == 0 || profile.Field == profile.Qualifier || profile.Qualifier == '\n' || profile.Field == '\n' {
		return ErrInvalidProfile
	}
	if !utf8.ValidRune(profile.Field) || !utf8.ValidRune(profile.Qualifier) || !utf8.ValidRune(profile.NewlineInField) ||
		profile.NewlineInField == profile.Field || profile.NewlineInField == profile.Qualifier {
		return ErrInvalidProfile
	}
	if !profile.HeaderRow && len(profile.Columns) == 0 {
		return fmt.Errorf("%w: headerless DAT requires declared columns", ErrInvalidProfile)
	}
	if err := validateDeclaredColumns(profile.Columns); err != nil {
		return err
	}
	return nil
}

func validateDeclaredColumns(columns []string) error {
	if err := validateRowValues(columns, 0); err != nil {
		return fmt.Errorf("%w: declared columns exceed header bounds", ErrInvalidProfile)
	}
	return nil
}

// consumeOptionalLF completes either a CR or CRLF record terminator.
func consumeOptionalLF(reader *bufio.Reader) error {
	next, err := reader.Peek(1)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("peek after CR: %w", err)
	}
	if next[0] == '\n' {
		if _, err := reader.Discard(1); err != nil {
			return fmt.Errorf("consume LF after CR: %w", err)
		}
	}
	return nil
}
