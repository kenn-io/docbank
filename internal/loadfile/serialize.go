package loadfile

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/transform"
)

// WriteDAT writes a complete DAT file after validating its contents and encoding.
// I/O errors may leave partial output. File callers must write to a temporary
// file and rename it only after this call and the file's Close both succeed.
func WriteDAT(destination io.Writer, records []Record, profile Profile) error {
	if err := validateDATProfile(profile); err != nil {
		return err
	}
	columns, err := validateWriterRecords(records, profile)
	if err != nil {
		return err
	}
	if profile.HeaderRow {
		for _, column := range columns {
			if strings.ContainsAny(column, "\r\n") || (profile.NewlineInField != 0 && strings.ContainsRune(column, profile.NewlineInField)) {
				return fmt.Errorf("%w: DAT column contains a newline or newline marker", ErrUnrepresentable)
			}
		}
	}
	return writeRecords(destination, records, columns, profile, writeDATRow)
}

// WriteCSV writes a complete CSV file. It has the same failure contract as WriteDAT.
// It preserves raw load-file text, including spreadsheet formula prefixes.
// Spreadsheet downloads require a separate cell-escaping policy.
// CRLF within a value is rejected because the CSV reader normalizes it to LF.
func WriteCSV(destination io.Writer, records []Record, profile Profile) error {
	if err := validateCSVProfile(profile); err != nil {
		return err
	}
	columns, err := validateWriterRecords(records, profile)
	if err != nil {
		return err
	}
	return writeRecords(destination, records, columns, profile, writeCSVRow)
}

func writeRecords(destination io.Writer, records []Record, columns []string, profile Profile, row func(io.Writer, []string, Profile) error) error {
	return writeLoadfile(destination, profile.Encoding, func(output io.Writer) error {
		if profile.HeaderRow && len(columns) > 0 {
			if err := row(output, columns, profile); err != nil {
				return err
			}
		}
		for _, record := range records {
			values := make([]string, len(record.Fields))
			for index, field := range record.Fields {
				values[index] = field.Raw
			}
			if err := row(output, values, profile); err != nil {
				return err
			}
		}
		return nil
	})
}

// WriteOPT writes a complete OPT file. It has the same failure contract as WriteDAT.
// It intentionally omits SourcePage, Boundary, and Rotation, which OPT cannot
// represent. Callers that need those ImageRef fields must retain them separately.
func WriteOPT(destination io.Writer, images []ImageRef, profile Profile) error {
	indexes, err := validateOPTProfile(profile)
	if err != nil {
		return err
	}
	return writeLoadfile(destination, profile.Encoding, func(output io.Writer) error {
		pageOrdinal := 0
		for rowIndex, image := range images {
			if image.DocumentBreak || rowIndex == 0 {
				pageOrdinal = 1
			} else {
				pageOrdinal++
			}
			if image.PageOrdinal != pageOrdinal || image.DeclaredPageCount < 0 {
				return fmt.Errorf("%w: OPT page %d contains inconsistent fields", ErrMalformedInput, rowIndex+1)
			}
			if strings.ContainsRune(image.RelPath, '\\') {
				return fmt.Errorf("%w: OPT page %d path contains a literal backslash", ErrUnrepresentable, rowIndex+1)
			}
			values := make([]string, len(profile.Columns))
			values[indexes["ImageKey"]] = image.ImageKey
			values[indexes["VolumeName"]] = image.Volume
			values[indexes["ImagePath"]] = strings.ReplaceAll(image.RelPath, "/", "\\")
			values[indexes["DocumentBreak"]] = optFlag(image.DocumentBreak)
			values[indexes["FolderBreak"]] = optFlag(image.FolderBreak)
			values[indexes["BoxBreak"]] = optFlag(image.BoxBreak)
			if image.DeclaredPageCount > 0 {
				values[indexes["PageCount"]] = strconv.Itoa(image.DeclaredPageCount)
			}
			for index, value := range values {
				if !utf8.ValidString(value) || strings.ContainsAny(value, ",\r\n") || len(value) > MaxFieldValueBytes {
					return fmt.Errorf("%w: OPT page %d field %s", ErrUnrepresentable, rowIndex+1, profile.Columns[index])
				}
			}
			if _, err := io.WriteString(output, strings.Join(values, ",")+"\r\n"); err != nil {
				return err
			}
		}
		return nil
	})
}

func validateWriterRecords(records []Record, profile Profile) ([]string, error) {
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
		if !utf8.ValidString(value) || (profile.NewlineInField != 0 && profile.NewlineInField != '\n' && strings.ContainsRune(value, profile.NewlineInField)) {
			return fmt.Errorf("DAT field %d: %w", index+1, ErrUnrepresentable)
		}
		encoded[index] = datCell(value, profile)
	}
	terminator := "\r\n"
	if profile.Field == '\r' || profile.Qualifier == '\r' {
		terminator = "\n"
	}
	_, err := io.WriteString(destination, strings.Join(encoded, string(profile.Field))+terminator)
	return err
}

func datCell(value string, profile Profile) string {
	qualifier := string(profile.Qualifier)
	value = strings.ReplaceAll(value, qualifier, qualifier+qualifier)
	if profile.NewlineInField != 0 {
		value = strings.ReplaceAll(value, "\n", string(profile.NewlineInField))
	}
	return qualifier + value + qualifier
}

func writeCSVRow(destination io.Writer, values []string, profile Profile) error {
	for _, value := range values {
		if !utf8.ValidString(value) || strings.Contains(value, "\r\n") {
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
	raw = append(raw[:len(raw)-1], '\r', '\n')
	if len(raw) > maxCSVRawRowBytes {
		return fmt.Errorf("%w: serialized CSV row exceeds %d decoded bytes", ErrLoadfileLimit, maxCSVRawRowBytes)
	}
	_, err := destination.Write(raw)
	return err
}

// writeLoadfile checks the complete output without retaining it, then writes it.
// The callback must start from the same input on each invocation.
func writeLoadfile(destination io.Writer, name string, write func(io.Writer) error) error {
	if destination == nil {
		return errors.New("load-file destination is nil")
	}
	codec, ok := loadfileEncodings[name]
	if !ok {
		return fmt.Errorf("%w: unsupported encoding %q", ErrInvalidProfile, name)
	}
	emit := func(destination io.Writer) error {
		buffered := bufio.NewWriter(destination)
		encoded := transform.NewWriter(buffered, codec.output.NewEncoder())
		if err := write(encoded); err != nil {
			return err
		}
		if err := encoded.Close(); err != nil {
			return fmt.Errorf("finish load-file encoding: %w", err)
		}
		return buffered.Flush()
	}
	var prefix loadfilePrefix
	if err := emit(&prefix); err != nil {
		if errors.Is(err, ErrMalformedInput) || errors.Is(err, ErrLoadfileLimit) || errors.Is(err, ErrUnrepresentable) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrUnrepresentable, err)
	}
	// Reuse the reader's BOM rules to reject content mistaken for an encoding
	// signature, including UTF-16LE followed by a leading NUL (a UTF-32 BOM).
	bom := bomReader{source: bytes.NewReader(prefix.bytes[:prefix.size]), declared: name}
	if err := bom.initialize(); err != nil {
		return fmt.Errorf("%w: output prefix: %w", ErrUnrepresentable, err)
	}
	return emit(destination)
}

// loadfilePrefix retains only the bytes needed to check an encoding signature.
type loadfilePrefix struct {
	bytes [4]byte
	size  int
}

func (p *loadfilePrefix) Write(data []byte) (int, error) {
	p.size += copy(p.bytes[p.size:], data)
	return len(data), nil
}

func optFlag(value bool) string {
	if value {
		return "Y"
	}
	return ""
}
