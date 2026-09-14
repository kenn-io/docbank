package loadfile

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
)

const maxCSVRawRowBytes = 1 << 20

func ScanCSV(source io.Reader, profile Profile, emit func(Record) error) ([]Diagnostic, error) {
	if err := validateCSVProfile(profile, emit); err != nil {
		return nil, err
	}
	decoder, err := Decoder(profile.Encoding)
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(decoder(source), validationReadSize)
	return scanDecodedRows(func() ([]string, bool, error) {
		raw, ok, err := readCSVRawRow(reader)
		if err != nil || !ok {
			return nil, ok, err
		}
		values, err := decodeCSVRow(raw, profile.Field)
		if err != nil {
			return nil, false, fmt.Errorf("decode CSV row: %w", err)
		}
		return values, true, nil
	}, profile, emit)
}

func ParseCSV(source io.Reader, profile Profile) ([]Record, []Diagnostic, error) {
	return collectRecords(func(emit func(Record) error) ([]Diagnostic, error) {
		return ScanCSV(source, profile, emit)
	})
}

type csvRawState uint8

const (
	csvRawFieldStart csvRawState = iota
	csvRawInBare
	csvRawInQualified
	csvRawAfterQualifier
)

func readCSVRawRow(reader *bufio.Reader) ([]byte, bool, error) {
	var raw bytes.Buffer
	state := csvRawFieldStart
	for {
		value, err := reader.ReadByte()
		if err != nil {
			if err == io.EOF && raw.Len() > 0 {
				return raw.Bytes(), true, nil
			}
			if err == io.EOF {
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("read CSV input: %w", err)
		}
		if raw.Len() == maxCSVRawRowBytes {
			return nil, false, fmt.Errorf("%w: CSV raw row exceeds %d bytes", ErrMalformedInput, maxCSVRawRowBytes)
		}
		if value == '\r' && state != csvRawInQualified {
			if err := consumeOptionalLF(reader); err != nil {
				return nil, false, fmt.Errorf("read CSV row terminator: %w", err)
			}
			value = '\n'
		}
		raw.WriteByte(value)

		switch state {
		case csvRawFieldStart:
			switch value {
			case '"':
				state = csvRawInQualified
			case ',':
			case '\n':
				return raw.Bytes(), true, nil
			default:
				state = csvRawInBare
			}
		case csvRawInBare:
			switch value {
			case ',':
				state = csvRawFieldStart
			case '\n':
				return raw.Bytes(), true, nil
			}
		case csvRawInQualified:
			if value == '"' {
				state = csvRawAfterQualifier
			}
		case csvRawAfterQualifier:
			switch value {
			case '"':
				state = csvRawInQualified
			case ',':
				state = csvRawFieldStart
			case '\n':
				return raw.Bytes(), true, nil
			default:
				state = csvRawInBare
			}
		}
	}
}

func decodeCSVRow(raw []byte, comma rune) ([]string, error) {
	reader := csv.NewReader(bytes.NewReader(raw))
	reader.Comma = comma
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = false
	values, err := reader.Read()
	if err == io.EOF {
		return []string{""}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: malformed CSV row: %w", ErrMalformedInput, err)
	}
	if _, err := reader.Read(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("%w: raw CSV boundary contains multiple records", ErrMalformedInput)
		}
		return nil, fmt.Errorf("%w: malformed CSV row: %w", ErrMalformedInput, err)
	}
	return values, nil
}

func validateCSVProfile(profile Profile, emit func(Record) error) error {
	if emit == nil || profile.Field != ',' || profile.Qualifier != '"' {
		return ErrInvalidProfile
	}
	if !profile.HeaderRow && len(profile.Columns) == 0 {
		return fmt.Errorf("%w: headerless CSV requires declared columns", ErrInvalidProfile)
	}
	if err := validateDeclaredColumns(profile.Columns); err != nil {
		return err
	}
	return nil
}
