package loadfile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const maxOPTLineBytes = 7*MaxFieldValueBytes + 6

var optColumns = [...]string{
	"ImageKey",
	"VolumeName",
	"ImagePath",
	"DocumentBreak",
	"FolderBreak",
	"BoxBreak",
	"PageCount",
}

func ParseOPT(source io.Reader, profile Profile) ([]ImageRef, []Diagnostic, error) {
	images := make([]ImageRef, 0, 100)
	diagnostics, err := scanOPT(source, profile, MaxRowsPerPage, func(image ImageRef) error {
		images = append(images, image)
		return nil
	})
	return images, diagnostics, err
}

// ScanOPT emits pages in source order without collecting a whole package.
func ScanOPT(source io.Reader, profile Profile, emit func(ImageRef) error) ([]Diagnostic, error) {
	return scanOPT(source, profile, 0, emit)
}

func scanOPT(source io.Reader, profile Profile, maxRows int, emit func(ImageRef) error) ([]Diagnostic, error) {
	if emit == nil {
		return nil, ErrInvalidProfile
	}
	indexes, err := validateOPTProfile(profile)
	if err != nil {
		return nil, err
	}
	decoder, err := Decoder(profile.Encoding)
	if err != nil {
		return nil, err
	}

	diagnostics := make([]Diagnostic, 0)
	pageOrdinal := 0
	rowOrdinal := 0
	scanner := bufio.NewScanner(decoder(source))
	scanner.Buffer(make([]byte, validationReadSize), maxOPTLineBytes+2)
	for scanner.Scan() {
		rowOrdinal++
		if maxRows > 0 && rowOrdinal > maxRows {
			return diagnostics, ErrLoadfileLimit
		}

		line := strings.TrimSuffix(scanner.Text(), "\r")
		values := strings.Split(line, ",")
		if len(values) != len(optColumns) {
			if err := appendDiagnosticBounded(&diagnostics, Diagnostic{
				Code:       "opt_field_count",
				Severity:   diagnosticSeverityBlocking,
				RowOrdinal: rowOrdinal,
				Detail:     fmt.Sprintf("OPT row has %d fields; profile declares %d", len(values), len(optColumns)),
			}); err != nil {
				return diagnostics, err
			}
			continue
		}
		for _, value := range values {
			if len(value) > MaxFieldValueBytes {
				return diagnostics, fmt.Errorf("%w: OPT row %d field exceeds %d decoded bytes", ErrMalformedInput, rowOrdinal, MaxFieldValueBytes)
			}
		}

		documentBreak, diagnostic := parseOPTFlag(values[indexes["DocumentBreak"]], "DocumentBreak", rowOrdinal)
		if diagnostic != nil {
			if err := appendDiagnosticBounded(&diagnostics, *diagnostic); err != nil {
				return diagnostics, err
			}
		}
		folderBreak, diagnostic := parseOPTFlag(values[indexes["FolderBreak"]], "FolderBreak", rowOrdinal)
		if diagnostic != nil {
			if err := appendDiagnosticBounded(&diagnostics, *diagnostic); err != nil {
				return diagnostics, err
			}
		}
		boxBreak, diagnostic := parseOPTFlag(values[indexes["BoxBreak"]], "BoxBreak", rowOrdinal)
		if diagnostic != nil {
			if err := appendDiagnosticBounded(&diagnostics, *diagnostic); err != nil {
				return diagnostics, err
			}
		}
		pageCount, diagnostic := parseOPTPageCount(values[indexes["PageCount"]], rowOrdinal)
		if diagnostic != nil {
			if err := appendDiagnosticBounded(&diagnostics, *diagnostic); err != nil {
				return diagnostics, err
			}
		}

		if documentBreak || pageOrdinal == 0 {
			pageOrdinal = 1
		} else {
			pageOrdinal++
		}
		if err := emit(ImageRef{
			ImageKey:          values[indexes["ImageKey"]],
			Volume:            values[indexes["VolumeName"]],
			RelPath:           strings.ReplaceAll(values[indexes["ImagePath"]], "\\", "/"),
			DocumentBreak:     documentBreak,
			FolderBreak:       folderBreak,
			BoxBreak:          boxBreak,
			PageOrdinal:       pageOrdinal,
			DeclaredPageCount: pageCount,
		}); err != nil {
			return diagnostics, err
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return diagnostics, fmt.Errorf("%w: OPT row exceeds %d bytes", ErrMalformedInput, maxOPTLineBytes)
		}
		return diagnostics, fmt.Errorf("read OPT input: %w", err)
	}
	return diagnostics, nil
}

func validateOPTProfile(profile Profile) (map[string]int, error) {
	if profile.Field != ',' || len(profile.Columns) != len(optColumns) {
		return nil, ErrInvalidProfile
	}
	indexes := make(map[string]int, len(profile.Columns))
	for index, column := range profile.Columns {
		if _, duplicate := indexes[column]; duplicate {
			return nil, fmt.Errorf("%w: duplicate OPT column %q", ErrInvalidProfile, column)
		}
		indexes[column] = index
	}
	for _, column := range optColumns {
		if _, ok := indexes[column]; !ok {
			return nil, fmt.Errorf("%w: missing OPT column %q", ErrInvalidProfile, column)
		}
	}
	return indexes, nil
}

func parseOPTFlag(raw, column string, rowOrdinal int) (bool, *Diagnostic) {
	switch raw {
	case "":
		return false, nil
	case "Y":
		return true, nil
	default:
		return false, &Diagnostic{
			Code:       "opt_flag_invalid",
			Severity:   diagnosticSeverityBlocking,
			Column:     column,
			RowOrdinal: rowOrdinal,
			Detail:     fmt.Sprintf("OPT %s must be Y or empty", column),
		}
	}
}

func parseOPTPageCount(raw string, rowOrdinal int) (int, *Diagnostic) {
	if raw == "" {
		return 0, nil
	}
	count, err := strconv.ParseUint(raw, 10, strconv.IntSize-1)
	if err == nil {
		return int(count), nil
	}
	return 0, &Diagnostic{
		Code:       "opt_page_count_invalid",
		Severity:   diagnosticSeverityBlocking,
		Column:     "PageCount",
		RowOrdinal: rowOrdinal,
		Detail:     "OPT PageCount must be an empty or unsigned decimal integer",
	}
}
