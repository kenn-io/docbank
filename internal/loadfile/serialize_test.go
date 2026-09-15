package loadfile

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadfileWriterDATEscapesQualifiersAndRefusesAmbiguousNewlineMarker(t *testing.T) {
	profile, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)

	cell, err := datCell("a þb\r\nc\rd\ne", profile)
	require.NoError(t, err)
	assert.Equal(t, "þa þþb®c®d®eþ", cell)

	_, err = datCell("literal ®", profile)
	require.ErrorIs(t, err, ErrUnrepresentable)
}

func TestExportProfilesAreClosedAndIndependent(t *testing.T) {
	profile, err := ReadExportProfile("export-dat-pdf-v1")
	require.NoError(t, err)
	assert.Equal(t, "dat-concordance-v1", profile.DAT)
	assert.Empty(t, profile.PageMap, "PDF-only DAT cannot identify interior PDF pages through OPT")
	assert.Equal(t, []string{"produced_pdf"}, profile.RequiredRoles)
	assert.Equal(t, []string{"supplied_text", "rendition_text", "native"}, profile.OptionalRoles)

	_, err = ReadExportProfile("export-dat-opt-tiff-v1")
	require.ErrorIs(t, err, ErrInvalidProfile)
	_, err = ReadExportProfile("export-dat-lfp-images-v1")
	require.ErrorIs(t, err, ErrInvalidProfile, "the LFP export profile waits for the LFP adapter")
	assert.Len(t, ExportProfiles(), 3)

	profile.RequiredRoles[0] = "mutated"
	profiles := ExportProfiles()
	profiles[0].OptionalRoles[0] = "mutated"
	again, err := ReadExportProfile("export-dat-pdf-v1")
	require.NoError(t, err)
	assert.Equal(t, []string{"produced_pdf"}, again.RequiredRoles)
	assert.Equal(t, []string{"supplied_text", "rendition_text", "native"}, again.OptionalRoles)
}

func TestLoadfileWritersRoundTripTheSharedModel(t *testing.T) {
	records, images := writerABModel()
	datProfile, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	optProfile, err := ReadProfile("opt-standard-v1")
	require.NoError(t, err)

	var dat bytes.Buffer
	require.NoError(t, WriteDAT(&dat, records, datProfile))
	readRecords, diagnostics, err := ParseDAT(bytes.NewReader(dat.Bytes()), datProfile)
	require.NoError(t, err)
	assert.Empty(t, diagnostics)
	require.Len(t, readRecords, len(records))
	for index := range records {
		assert.Equal(t, records[index].ColumnOrder, readRecords[index].ColumnOrder)
		require.Len(t, readRecords[index].Fields, len(records[index].Fields))
		for fieldIndex := range records[index].Fields {
			assert.Equal(t, records[index].Fields[fieldIndex].Column, readRecords[index].Fields[fieldIndex].Column)
			assert.Equal(t, records[index].Fields[fieldIndex].Raw, readRecords[index].Fields[fieldIndex].Raw)
		}
	}

	var opt bytes.Buffer
	require.NoError(t, WriteOPT(&opt, images, optProfile))
	readImages, diagnostics, err := ParseOPT(bytes.NewReader(opt.Bytes()), optProfile)
	require.NoError(t, err)
	assert.Empty(t, diagnostics)
	assert.Equal(t, images, readImages)
}

func TestLoadfileWriterCSVUsesRFC4180AndStrictEncoding(t *testing.T) {
	profile, err := ReadProfile("csv-rfc4180-v1")
	require.NoError(t, err)
	records := []Record{{
		ColumnOrder: []string{"ID", "TITLE"},
		Fields: []Field{
			{Column: "ID", Ordinal: 0, Raw: "A"},
			{Column: "TITLE", Ordinal: 1, Raw: "comma, quote \" and\nnewline"},
		},
	}}

	var output bytes.Buffer
	require.NoError(t, WriteCSV(&output, records, profile))
	assert.Contains(t, output.String(), "\r\n")
	parsed, diagnostics, err := ParseCSV(bytes.NewReader(output.Bytes()), profile)
	require.NoError(t, err)
	assert.Empty(t, diagnostics)
	require.Len(t, parsed, 1)
	assert.Equal(t, records[0].Fields[1].Raw, parsed[0].Fields[1].Raw)

	profile.Encoding = "windows-1252"
	records[0].Fields[1].Raw = "中文"
	require.ErrorIs(t, WriteCSV(&bytes.Buffer{}, records, profile), ErrUnrepresentable)
}

func TestLoadfileWriterCSVPreservesBareCarriageReturn(t *testing.T) {
	profile, err := ReadProfile("csv-rfc4180-v1")
	require.NoError(t, err)
	records := []Record{{
		ColumnOrder: []string{"ID", "TITLE"},
		Fields: []Field{
			{Column: "ID", Ordinal: 0, Raw: "A"},
			{Column: "TITLE", Ordinal: 1, Raw: "before\rafter"},
		},
	}}

	var output bytes.Buffer
	require.NoError(t, WriteCSV(&output, records, profile))
	assert.Contains(t, output.String(), "\"before\rafter\"")
	parsed, diagnostics, err := ParseCSV(bytes.NewReader(output.Bytes()), profile)
	require.NoError(t, err)
	assert.Empty(t, diagnostics)
	require.Len(t, parsed, 1)
	assert.Equal(t, records[0].Fields[1].Raw, parsed[0].Fields[1].Raw)
}

func TestLoadfileWriterCSVRefusesExpandedRowBeyondReaderBound(t *testing.T) {
	profile, err := ReadProfile("csv-rfc4180-v1")
	require.NoError(t, err)
	const columns = maxDecodedRowBytes / MaxFieldValueBytes
	record := Record{
		ColumnOrder: make([]string, columns),
		Fields:      make([]Field, columns),
	}
	for index := range columns {
		column := fmt.Sprintf("FIELD_%02d", index)
		record.ColumnOrder[index] = column
		record.Fields[index] = Field{
			Column: column, Ordinal: index, Raw: strings.Repeat("\"", MaxFieldValueBytes),
		}
	}

	require.ErrorIs(t, WriteCSV(&bytes.Buffer{}, []Record{record}, profile), ErrLoadfileLimit)
}

func TestLoadfileWriterDATRefusesUnrepresentableAndInconsistentValues(t *testing.T) {
	profile, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	profile.Encoding = "windows-1252"
	records := []Record{{
		ColumnOrder: []string{"TITLE"},
		Fields:      []Field{{Column: "TITLE", Ordinal: 0, Raw: "中文"}},
	}}
	require.ErrorIs(t, WriteDAT(&bytes.Buffer{}, records, profile), ErrUnrepresentable)

	profile.Encoding = "utf-8"
	records[0].Fields[0].Column = "OTHER"
	require.ErrorIs(t, WriteDAT(&bytes.Buffer{}, records, profile), ErrMalformedInput)
	records[0].Fields[0].Column = "TITLE"
	records[0].Fields[0].Ordinal = 1
	require.ErrorIs(t, WriteDAT(&bytes.Buffer{}, records, profile), ErrMalformedInput)
}

func TestLoadfileWriterWritesOneDeclaredBOM(t *testing.T) {
	records, _ := writerABModel()
	for _, test := range []struct {
		encoding string
		bom      []byte
	}{
		{encoding: "utf-8-bom", bom: []byte{0xef, 0xbb, 0xbf}},
		{encoding: "utf-16le", bom: []byte{0xff, 0xfe}},
		{encoding: "utf-16be", bom: []byte{0xfe, 0xff}},
	} {
		t.Run(test.encoding, func(t *testing.T) {
			profile, err := ReadProfile("dat-concordance-v1")
			require.NoError(t, err)
			profile.Encoding = test.encoding
			var output bytes.Buffer
			require.NoError(t, WriteDAT(&output, records, profile))
			assert.True(t, bytes.HasPrefix(output.Bytes(), test.bom))
			assert.Equal(t, 1, bytes.Count(output.Bytes(), test.bom))

			parsed, diagnostics, err := ParseDAT(bytes.NewReader(output.Bytes()), profile)
			require.NoError(t, err)
			assert.Empty(t, diagnostics)
			assert.Len(t, parsed, len(records))
		})
	}
}

func TestLoadfileWriterOPTUsesDeclaredColumnsAndRejectsAmbiguousCells(t *testing.T) {
	profile, err := ReadProfile("opt-pagecount5-v1")
	require.NoError(t, err)
	image := ImageRef{
		ImageKey: "IMG1", Volume: "VOL1", RelPath: "IMAGES/A.TIF",
		DocumentBreak: true, PageOrdinal: 1, DeclaredPageCount: 2,
	}
	var output bytes.Buffer
	require.NoError(t, WriteOPT(&output, []ImageRef{image}, profile))
	assert.Equal(t, "IMG1,VOL1,IMAGES\\A.TIF,Y,2,,\n", output.String())

	image.RelPath = "IMAGES/A,1.TIF"
	require.ErrorIs(t, WriteOPT(&bytes.Buffer{}, []ImageRef{image}, profile), ErrUnrepresentable)
	image.RelPath = "IMAGES/A.TIF"
	image.DeclaredPageCount = -1
	require.ErrorIs(t, WriteOPT(&bytes.Buffer{}, []ImageRef{image}, profile), ErrMalformedInput)
}

func writerABModel() ([]Record, []ImageRef) {
	columns := []string{"BEGBATES", "TITLE", "PDFPATH"}
	record := func(values ...string) Record {
		fields := make([]Field, len(values))
		for index, value := range values {
			fields[index] = Field{Column: columns[index], Ordinal: index, Raw: value}
		}
		return Record{ColumnOrder: append([]string(nil), columns...), Fields: fields}
	}
	records := []Record{
		record("OUR000041", "Alpha þ memo", "PDF/A.pdf"),
		record("OUR000043", "Beta\nreport", "PDF/B.pdf"),
	}
	images := []ImageRef{
		{ImageKey: "OUR000041", Volume: "VOL001", RelPath: "IMAGES/A-1.tif", DocumentBreak: true, PageOrdinal: 1, DeclaredPageCount: 2},
		{ImageKey: "OUR000042", Volume: "VOL001", RelPath: "IMAGES/A-2.tif", PageOrdinal: 2},
		{ImageKey: "OUR000043", Volume: "VOL001", RelPath: "IMAGES/B-1.tif", DocumentBreak: true, PageOrdinal: 1, DeclaredPageCount: 1},
	}
	return records, images
}

func TestLoadfileWriterRawValuesRemainAuthority(t *testing.T) {
	profile, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	record := Record{
		ColumnOrder: []string{"VALUE"},
		Fields: []Field{{
			Column: "VALUE", Ordinal: 0, Raw: "sender literal",
			Value: Value{Kind: "list", List: strings.Fields("normalized values")},
		}},
	}
	var output bytes.Buffer
	require.NoError(t, WriteDAT(&output, []Record{record}, profile))
	assert.Contains(t, output.String(), "sender literal")
	assert.NotContains(t, output.String(), "normalized")
}
