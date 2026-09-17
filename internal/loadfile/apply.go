package loadfile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// ApplyMapping binds confirmed source ordinals to the closed field catalog and
// derives the intermediate record fields that later import consumes.
// reserve charges estimated added memory to the caller's shared package budget
// before allocating mapped values, family lists, or appended fields.
func ApplyMapping(records []Record, mapping Mapping, profile Profile, reserve func(int64) error) ([]Diagnostic, error) {
	diagnostics := make([]Diagnostic, 0)
	byOrdinal := make(map[int]MappingColumn, len(mapping.Columns))
	for _, column := range mapping.Columns {
		if column.SourceOrdinal != nil {
			byOrdinal[*column.SourceOrdinal] = column
		}
	}
	for recordIndex := range records {
		record := &records[recordIndex]
		if err := reserve(64); err != nil {
			return diagnostics, err
		}
		recordDiagnosticStart := len(diagnostics)
		rawByOrdinal := make(map[int]string, len(record.Fields))
		for _, field := range record.Fields {
			rawByOrdinal[field.Ordinal] = field.Raw
		}
		for fieldIndex := range record.Fields {
			field := &record.Fields[fieldIndex]
			column, ok := byOrdinal[field.Ordinal]
			if len(mapping.Columns) == 0 {
				canonical := conventionalField(field.Column)
				column, ok = MappingColumn{Canonical: &canonical}, canonical != ""
			}
			if !ok || column.Canonical == nil {
				continue
			}
			if err := reserve(int64(len(*column.Canonical))); err != nil {
				return diagnostics, err
			}
			field.Canonical = *column.Canonical
			before := len(diagnostics)
			parseRaw := field.Raw
			if column.PairedDateOrdinal != nil && field.Raw != "" {
				if pairedDate := rawByOrdinal[*column.PairedDateOrdinal]; pairedDate != "" {
					if err := reserve(int64(len(pairedDate) + 1 + len(field.Raw))); err != nil {
						return diagnostics, err
					}
					parseRaw = pairedDate + " " + field.Raw
				}
			}
			value, err := mappedValue(parseRaw, column, profile, &diagnostics, reserve)
			if err != nil {
				return diagnostics, err
			}
			field.Value = value
			for index := before; index < len(diagnostics); index++ {
				diagnostics[index].Column = field.Column
			}
			if err := applyMappedField(record, field.Canonical, field.Raw, field.Value, reserve); err != nil {
				return diagnostics, err
			}
		}
		hasCustodian := false
		if mapping.CustodianColumn != "" {
			for fieldIndex := range record.Fields {
				if record.Fields[fieldIndex].Column == mapping.CustodianColumn {
					if err := reserve(int64(len("loadfile.custodian") + 4 + len(record.Fields[fieldIndex].Raw))); err != nil {
						return diagnostics, err
					}
					record.Fields[fieldIndex].Canonical = "loadfile.custodian"
					record.Fields[fieldIndex].Value = Value{Kind: "text", Text: record.Fields[fieldIndex].Raw}
					hasCustodian = record.Fields[fieldIndex].Raw != ""
				}
			}
		}
		if mapping.DefaultCustodian != "" && !hasCustodian {
			if err := reserve(int64(192 + len("loadfile.custodian") + 4 + 2*len(mapping.DefaultCustodian))); err != nil {
				return diagnostics, err
			}
			record.Fields = append(record.Fields, Field{Ordinal: len(record.Fields), Canonical: "loadfile.custodian", Raw: mapping.DefaultCustodian, Value: Value{Kind: "text", Text: mapping.DefaultCustodian}})
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("package-row/v1\x00%s\x00%d\x00%s", record.LoadFile, record.RowOrdinal, record.DocID)))
		record.RowID = hex.EncodeToString(sum[:])
		for index := recordDiagnosticStart; index < len(diagnostics); index++ {
			diagnostics[index].LoadFile = record.LoadFile
			diagnostics[index].RowID = record.RowID
			diagnostics[index].RowOrdinal = record.RowOrdinal
		}
	}
	return diagnostics, nil
}

func mappedValue(raw string, column MappingColumn, profile Profile, diagnostics *[]Diagnostic, reserve func(int64) error) (Value, error) {
	if raw == "" {
		return Value{Kind: "text"}, reserve(4)
	}
	timeField := strings.HasPrefix(*column.Canonical, "loadfile.date.") || strings.HasPrefix(*column.Canonical, "loadfile.time.") || strings.HasPrefix(*column.Canonical, "loadfile.calendar.")
	size := int64(4 + len(raw))
	if timeField {
		// Include the time claim and derived date/time strings before parsing.
		size += int64(256 + len(raw) + len(profile.DeclaredTimezone) + len(column.Timezone))
	} else if column.MultiValue {
		size += 16 * int64(strings.Count(raw, ";")+1)
	}
	if err := reserve(size); err != nil {
		return Value{}, err
	}
	if timeField {
		dateProfile := profile
		if column.DateFormat != "" {
			dateProfile.DateFormat = column.DateFormat
		}
		if column.Timezone != "" {
			dateProfile.DeclaredTimezone = column.Timezone
		}
		claim, findings := ParseTimeClaim(raw, dateProfile)
		for _, finding := range findings {
			if err := appendDiagnosticBounded(diagnostics, finding); err != nil {
				return Value{}, err
			}
		}
		return Value{Kind: "time", Time: &claim}, nil
	}
	if column.MultiValue {
		return Value{Kind: "list", List: strings.Split(raw, ";")}, nil
	}
	return Value{Kind: "text", Text: raw}, nil
}

func applyMappedField(record *Record, canonical, raw string, value Value, reserve func(int64) error) error {
	var size int64
	switch canonical {
	case "loadfile.document.id", "loadfile.family.parent", "loadfile.family.id":
		size = int64(len(raw))
	case "loadfile.family.children":
		if raw != "" {
			size = int64(len(raw)) + 16*int64(strings.Count(raw, ";")+1)
		}
	case "loadfile.file.native", "loadfile.file.produced_pdf", "loadfile.file.supplied_text":
		if raw != "" {
			size = int64(320 + 2*len(raw))
		}
	}
	if err := reserve(size); err != nil {
		return err
	}
	switch canonical {
	case "loadfile.document.id":
		record.DocID = raw
	case "loadfile.family.parent":
		record.Family.ParentDocID = raw
	case "loadfile.family.children":
		if value.Kind == "list" {
			record.Family.AttachmentDocIDs = append([]string(nil), value.List...)
		} else if raw != "" {
			record.Family.AttachmentDocIDs = strings.Split(raw, ";")
		}
	case "loadfile.family.id":
		record.Family.GroupID = raw
	case "loadfile.file.native":
		appendMappedFile(record, "native", raw)
	case "loadfile.file.produced_pdf":
		appendMappedFile(record, "produced_pdf", raw)
	case "loadfile.file.supplied_text":
		appendMappedFile(record, "supplied_text", raw)
	}
	return nil
}

func appendMappedFile(record *Record, role, declared string) {
	if declared == "" {
		return
	}
	clean := strings.ReplaceAll(declared, `\`, "/")
	record.Files = append(record.Files, FileRef{Role: role, RelPath: clean, Declared: declared, Status: "available"})
}

// NormalizeFileReferences binds declared paths to the actual volume catalog.
// Volume names are data, so callers must not infer them from a VOL prefix.
func NormalizeFileReferences(records []Record, volumes []Volume) {
	byName := make(map[string]bool, len(volumes))
	for _, volume := range volumes {
		byName[volume.Name] = true
	}
	for recordIndex := range records {
		record := &records[recordIndex]
		for fileIndex := range record.Files {
			ref := &record.Files[fileIndex]
			clean := strings.ReplaceAll(ref.RelPath, `\`, "/")
			first, rest, found := strings.Cut(clean, "/")
			if found && byName[first] {
				ref.Volume, ref.RelPath = first, rest
				continue
			}
			ref.RelPath = clean
			if ref.Volume == "" && len(volumes) == 1 {
				ref.Volume = volumes[0].Name
			}
		}
	}
}

func conventionalField(column string) string {
	switch strings.ToUpper(column) {
	case "DOCID", "BEGDOC":
		return "loadfile.document.id"
	case "PARENT", "PARENTID":
		return "loadfile.family.parent"
	case "NATIVE", "NATIVEFILE", "NATIVEPATH":
		return "loadfile.file.native"
	case "TEXT", "TEXTPATH":
		return "loadfile.file.supplied_text"
	}
	return ""
}
