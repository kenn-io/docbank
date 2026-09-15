package loadfile

import (
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

func writerEmit(Record) error { return nil }

func datCell(value string, profile Profile) (string, error) {
	if profile.Field == 0 || profile.Qualifier == 0 || profile.NewlineInField == 0 ||
		profile.Field == profile.Qualifier || profile.Field == profile.NewlineInField ||
		profile.Qualifier == profile.NewlineInField ||
		strings.ContainsRune("\r\n", profile.Field) ||
		strings.ContainsRune("\r\n", profile.Qualifier) ||
		strings.ContainsRune("\r\n", profile.NewlineInField) {
		return "", ErrInvalidProfile
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, profile.NewlineInField) {
		return "", ErrUnrepresentable
	}
	qualifier := string(profile.Qualifier)
	value = strings.ReplaceAll(value, qualifier, qualifier+qualifier)
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.ReplaceAll(value, "\n", string(profile.NewlineInField))
	return qualifier + value + qualifier, nil
}

func WriteDAT(destination io.Writer, records []Record, profile Profile) error {
	if destination == nil || profile.ID == "csv-rfc4180-v1" {
		return ErrInvalidProfile
	}
	if err := validateDATProfile(profile, writerEmit); err != nil {
		return err
	}
	if _, err := datCell("", profile); err != nil {
		return err
	}
	columns, err := validateWriterRecords(records, profile)
	if err != nil {
		return err
	}
	if err := writeEncodingPreamble(destination, profile.Encoding); err != nil {
		return err
	}
	if profile.HeaderRow && len(columns) > 0 {
		if err := writeDATRow(destination, columns, profile); err != nil {
			return err
		}
	}
	for _, record := range records {
		values := make([]string, len(record.Fields))
		for index, field := range record.Fields {
			values[index] = field.Raw
		}
		if err := writeDATRow(destination, values, profile); err != nil {
			return err
		}
	}
	return nil
}

func WriteCSV(destination io.Writer, records []Record, profile Profile) error {
	if destination == nil {
		return ErrInvalidProfile
	}
	if err := validateCSVProfile(profile, writerEmit); err != nil {
		return err
	}
	columns, err := validateWriterRecords(records, profile)
	if err != nil {
		return err
	}
	if err := writeEncodingPreamble(destination, profile.Encoding); err != nil {
		return err
	}
	if profile.HeaderRow && len(columns) > 0 {
		if err := writeCSVRow(destination, columns, profile); err != nil {
			return err
		}
	}
	for _, record := range records {
		values := make([]string, len(record.Fields))
		for index, field := range record.Fields {
			values[index] = field.Raw
		}
		if err := writeCSVRow(destination, values, profile); err != nil {
			return err
		}
	}
	return nil
}

func WriteOPT(destination io.Writer, images []ImageRef, profile Profile) error {
	if destination == nil {
		return ErrInvalidProfile
	}
	if _, err := validateOPTProfile(profile); err != nil {
		return err
	}
	if len(images) > MaxRowsPerPage {
		return ErrLoadfileLimit
	}
	if err := writeEncodingPreamble(destination, profile.Encoding); err != nil {
		return err
	}
	pageOrdinal := 0
	for rowIndex, image := range images {
		if image.DocumentBreak || rowIndex == 0 {
			pageOrdinal = 1
		} else {
			pageOrdinal++
		}
		if image.PageOrdinal != pageOrdinal || image.DeclaredPageCount < 0 ||
			image.SourcePage != 0 || image.Boundary != "" || image.Rotation != 0 {
			return fmt.Errorf("%w: OPT page %d contains inconsistent or unsupported fields", ErrMalformedInput, rowIndex+1)
		}
		pageCount := ""
		if image.DeclaredPageCount > 0 {
			pageCount = strconv.Itoa(image.DeclaredPageCount)
		}
		fields := map[string]string{
			"ImageKey":      image.ImageKey,
			"VolumeName":    image.Volume,
			"ImagePath":     strings.ReplaceAll(image.RelPath, "/", "\\"),
			"DocumentBreak": optFlag(image.DocumentBreak),
			"FolderBreak":   optFlag(image.FolderBreak),
			"BoxBreak":      optFlag(image.BoxBreak),
			"PageCount":     pageCount,
		}
		values := make([]string, len(profile.Columns))
		for index, column := range profile.Columns {
			value := fields[column]
			if !utf8.ValidString(value) || strings.ContainsAny(value, ",\r\n") || len(value) > MaxFieldValueBytes {
				return fmt.Errorf("%w: OPT page %d field %s", ErrUnrepresentable, rowIndex+1, column)
			}
			values[index] = value
		}
		if err := writeSeparatedRow(destination, values, ",", profile.Encoding); err != nil {
			return err
		}
	}
	return nil
}

func validateWriterRecords(records []Record, profile Profile) ([]string, error) {
	if len(records) > MaxRowsPerPage {
		return nil, ErrLoadfileLimit
	}
	columns := append([]string(nil), profile.Columns...)
	if len(columns) == 0 && len(records) > 0 {
		columns = append(columns, records[0].ColumnOrder...)
	}
	if len(records) > 0 && len(columns) == 0 {
		return nil, fmt.Errorf("%w: output columns are empty", ErrMalformedInput)
	}
	if err := validateDeclaredColumns(columns); err != nil {
		return nil, err
	}
	for recordIndex, record := range records {
		if !slices.Equal(record.ColumnOrder, columns) || len(record.Fields) != len(columns) {
			return nil, fmt.Errorf("%w: record %d does not match the declared columns", ErrMalformedInput, recordIndex+1)
		}
		values := make([]string, len(record.Fields))
		for fieldIndex, field := range record.Fields {
			if field.Ordinal != fieldIndex || field.Column != columns[fieldIndex] {
				return nil, fmt.Errorf("%w: record %d field %d has inconsistent identity", ErrMalformedInput, recordIndex+1, fieldIndex+1)
			}
			values[fieldIndex] = field.Raw
		}
		if err := validateRowValues(values, recordIndex+1); err != nil {
			return nil, err
		}
	}
	return columns, nil
}

func writeDATRow(destination io.Writer, values []string, profile Profile) error {
	encoded := make([]string, len(values))
	for index, value := range values {
		cell, err := datCell(value, profile)
		if err != nil {
			return fmt.Errorf("DAT field %d: %w", index+1, err)
		}
		encoded[index] = cell
	}
	return writeSeparatedRow(destination, encoded, string(profile.Field), profile.Encoding)
}

func writeCSVRow(destination io.Writer, values []string, profile Profile) error {
	for _, value := range values {
		if !utf8.ValidString(value) {
			return ErrUnrepresentable
		}
	}
	var row bytes.Buffer
	writer := csv.NewWriter(&row)
	writer.Comma = profile.Field
	if err := writer.Write(values); err != nil {
		return fmt.Errorf("encode CSV row: %w", err)
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return fmt.Errorf("encode CSV row: %w", err)
	}
	raw := row.Bytes()
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return fmt.Errorf("%w: CSV row has no terminator", ErrUnrepresentable)
	}
	raw = append(raw[:len(raw)-1], '\r', '\n')
	if len(raw) > maxCSVRawRowBytes {
		return fmt.Errorf("%w: serialized CSV row exceeds %d decoded bytes", ErrLoadfileLimit, maxCSVRawRowBytes)
	}
	return writeEncodedString(destination, profile.Encoding, string(raw))
}

func writeSeparatedRow(destination io.Writer, values []string, separator, encoding string) error {
	for index, value := range values {
		if index > 0 {
			if err := writeEncodedString(destination, encoding, separator); err != nil {
				return err
			}
		}
		if err := writeEncodedString(destination, encoding, value); err != nil {
			return err
		}
	}
	return writeEncodedString(destination, encoding, "\n")
}

func writeEncodingPreamble(destination io.Writer, encoding string) error {
	var preamble []byte
	switch encoding {
	case "utf-8", "windows-1252", "iso-8859-1":
	case "utf-8-bom":
		preamble = []byte{0xef, 0xbb, 0xbf}
	case "utf-16le":
		preamble = []byte{0xff, 0xfe}
	case "utf-16be":
		preamble = []byte{0xfe, 0xff}
	default:
		return fmt.Errorf("%w: unsupported encoding %q", ErrInvalidProfile, encoding)
	}
	return writeAll(destination, preamble)
}

func writeEncodedString(destination io.Writer, encoding, value string) error {
	encoded, err := encodeString(encoding, value)
	if err != nil {
		return err
	}
	return writeAll(destination, encoded)
}

func encodeString(encoding, value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, ErrUnrepresentable
	}
	switch encoding {
	case "utf-8", "utf-8-bom":
		return []byte(value), nil
	case "utf-16le", "utf-16be":
		units := utf16.Encode([]rune(value))
		encoded := make([]byte, len(units)*2)
		var order binary.ByteOrder = binary.LittleEndian
		if encoding == "utf-16be" {
			order = binary.BigEndian
		}
		for index, unit := range units {
			order.PutUint16(encoded[index*2:], unit)
		}
		return encoded, nil
	case "windows-1252":
		encoded, err := charmap.Windows1252.NewEncoder().Bytes([]byte(value))
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUnrepresentable, err)
		}
		return encoded, nil
	case "iso-8859-1":
		encoded, err := charmap.ISO8859_1.NewEncoder().Bytes([]byte(value))
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUnrepresentable, err)
		}
		return encoded, nil
	default:
		return nil, fmt.Errorf("%w: unsupported encoding %q", ErrInvalidProfile, encoding)
	}
}

func writeAll(destination io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := destination.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func optFlag(value bool) string {
	if value {
		return "Y"
	}
	return ""
}
