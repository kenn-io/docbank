package loadfile

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	assert.Equal(t, "IMG1,VOL1,IMAGES\\A.TIF,Y,2,,\r\n", output.String())

	image.RelPath = "IMAGES/A,1.TIF"
	require.ErrorIs(t, WriteOPT(&bytes.Buffer{}, []ImageRef{image}, profile), ErrUnrepresentable)
	image.RelPath = "IMAGES/A.TIF"
	image.DeclaredPageCount = -1
	require.ErrorIs(t, WriteOPT(&bytes.Buffer{}, []ImageRef{image}, profile), ErrMalformedInput)
}

func TestLoadfileWriterOPTOmitsFieldsOutsideTheFormat(t *testing.T) {
	profile, err := ReadProfile("opt-standard-v1")
	require.NoError(t, err)
	image := ImageRef{
		ImageKey: "IMG1", Volume: "VOL1", RelPath: "IMAGES/A.TIF",
		DocumentBreak: true, FolderBreak: true, BoxBreak: true,
		PageOrdinal: 1, DeclaredPageCount: 2,
		SourcePage: 3, Boundary: "document", Rotation: 90,
	}
	var output bytes.Buffer
	require.NoError(t, WriteOPT(&output, []ImageRef{image}, profile))
	parsed, diagnostics, err := ParseOPT(&output, profile)
	require.NoError(t, err)
	assert.Empty(t, diagnostics)
	assert.Equal(t, []ImageRef{{
		ImageKey: "IMG1", Volume: "VOL1", RelPath: "IMAGES/A.TIF",
		DocumentBreak: true, FolderBreak: true, BoxBreak: true,
		PageOrdinal: 1, DeclaredPageCount: 2,
	}}, parsed)
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

func TestLoadfileWritersBeyondReaderPage(t *testing.T) {
	for _, tc := range []struct {
		id    string
		write func(io.Writer, []Record, Profile) error
		scan  func(io.Reader, Profile, func(Record) error) ([]Diagnostic, error)
	}{{"dat-concordance-v1", WriteDAT, ScanDAT}, {"csv-rfc4180-v1", WriteCSV, ScanCSV}} {
		t.Run(tc.id, func(t *testing.T) {
			profile, err := ReadProfile(tc.id)
			require.NoError(t, err)
			records := make([]Record, MaxRowsPerPage+1)
			for i := range records {
				records[i] = Record{ColumnOrder: []string{"ID"}, Fields: []Field{{Column: "ID", Raw: strconv.Itoa(i)}}}
			}
			var output bytes.Buffer
			require.NoError(t, tc.write(&output, records, profile))
			count := 0
			diagnostics, err := tc.scan(&output, profile, func(record Record) error {
				assert.Equal(t, records[count].Fields, record.Fields)
				count++
				return nil
			})
			require.NoError(t, err)
			assert.Empty(t, diagnostics)
			assert.Equal(t, len(records), count)
		})
	}
	t.Run("OPT", func(t *testing.T) {
		profile, err := ReadProfile("opt-standard-v1")
		require.NoError(t, err)
		images := make([]ImageRef, MaxRowsPerPage+1)
		for i := range images {
			images[i] = ImageRef{ImageKey: strconv.Itoa(i), RelPath: "images/page.tif", PageOrdinal: i + 1}
		}
		var output bytes.Buffer
		require.NoError(t, WriteOPT(&output, images, profile))
		count := 0
		diagnostics, err := ScanOPT(&output, profile, func(image ImageRef) error {
			assert.Equal(t, images[count], image)
			count++
			return nil
		})
		require.NoError(t, err)
		assert.Empty(t, diagnostics)
		assert.Equal(t, len(images), count)
	})
}

func TestLoadfileWritersRejectLossyValuesBeforeOutput(t *testing.T) {
	for _, tc := range []struct {
		name, id, encoding, column, value string
		write                             func(io.Writer, []Record, Profile) error
	}{
		{"CSV CRLF", "csv-rfc4180-v1", "utf-8", "TITLE", "a\r\nb", WriteCSV},
		{"CSV header CRLF", "csv-rfc4180-v1", "utf-8", "A\r\nB", "value", WriteCSV},
		{"CSV leading BOM", "csv-rfc4180-v1", "utf-8", "\ufeffTITLE", "value", WriteCSV},
		{"CSV leading NUL", "csv-rfc4180-v1", "utf-16le", "\x00TITLE", "value", WriteCSV},
		{"CSV encoding", "csv-rfc4180-v1", "windows-1252", "TITLE", "中文", WriteCSV},
		{"DAT encoding", "dat-concordance-v1", "windows-1252", "TITLE", "中文", WriteDAT},
		{"DAT marker", "dat-concordance-v1", "utf-8-bom", "TITLE", "literal ®", WriteDAT},
		{"DAT header CR", "dat-concordance-v1", "utf-8", "A\rB", "value", WriteDAT},
		{"DAT header LF", "dat-concordance-v1", "utf-8", "A\nB", "value", WriteDAT},
		{"DAT header marker", "dat-concordance-v1", "utf-8", "A®B", "value", WriteDAT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := ReadProfile(tc.id)
			require.NoError(t, err)
			profile.Encoding = tc.encoding
			records := []Record{
				{ColumnOrder: []string{tc.column}, Fields: []Field{{Column: tc.column, Raw: strings.Repeat("a", 5000)}}},
				{ColumnOrder: []string{tc.column}, Fields: []Field{{Column: tc.column, Raw: tc.value}}},
			}
			var output bytes.Buffer
			err = tc.write(&output, records, profile)
			require.ErrorIs(t, err, ErrUnrepresentable)
			assert.Zero(t, output.Len(), "rejected values must not leave a partial file")
		})
	}
	for _, tc := range []struct{ name, encoding, key, path string }{
		{"OPT backslash", "utf-8", "IMG", `dir\x/b.tif`},
		{"OPT leading BOM", "utf-8", "\ufeffIMG", "images/page.tif"},
		{"OPT leading NUL", "utf-16le", "\x00IMG", "images/page.tif"},
		{"OPT encoding", "windows-1252", "IMG", "images/中文.tif"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := ReadProfile("opt-standard-v1")
			require.NoError(t, err)
			profile.Encoding = tc.encoding
			images := []ImageRef{
				{ImageKey: tc.key, RelPath: "images/page.tif", PageOrdinal: 1},
				{ImageKey: "IMG2", RelPath: tc.path, PageOrdinal: 2},
			}
			var output bytes.Buffer
			err = WriteOPT(&output, images, profile)
			require.ErrorIs(t, err, ErrUnrepresentable)
			assert.Zero(t, output.Len())
		})
	}
}

func TestLoadfileWriterDATPreservesReaderValues(t *testing.T) {
	for _, marker := range []rune{'®', 0, '\r'} {
		t.Run(fmt.Sprintf("marker-%U", marker), func(t *testing.T) {
			profile, err := ReadProfile("dat-concordance-v1")
			require.NoError(t, err)
			profile.NewlineInField = marker
			records, diagnostics, err := ParseDAT(strings.NewReader("þTITLEþ\nþa\r\nb\rc\ndþ\n"), profile)
			require.NoError(t, err)
			require.Empty(t, diagnostics)
			var output bytes.Buffer
			require.NoError(t, WriteDAT(&output, records, profile))
			parsed, diagnostics, err := ParseDAT(&output, profile)
			require.NoError(t, err)
			assert.Empty(t, diagnostics)
			assert.Equal(t, records, parsed)
		})
	}
}

func TestLoadfileWriterDATHeaderlessColumnNames(t *testing.T) {
	profile, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	profile.HeaderRow = false
	profile.Columns = []string{"WITH\rCR", "WITH\nLF", "WITH®MARKER"}
	const input = "þfirstþ\x14þsecondþ\x14þthirdþ\r\n"
	records, diagnostics, err := ParseDAT(strings.NewReader(input), profile)
	require.NoError(t, err)
	require.Empty(t, diagnostics)

	var output bytes.Buffer
	require.NoError(t, WriteDAT(&output, records, profile))
	assert.Equal(t, input, output.String())
	parsed, diagnostics, err := ParseDAT(&output, profile)
	require.NoError(t, err)
	assert.Empty(t, diagnostics)
	assert.Equal(t, records, parsed)
}

func TestLoadfileWritersPropagateDestinationErrors(t *testing.T) {
	records, images := writerABModel()
	for _, tc := range []struct {
		id    string
		write func(io.Writer, Profile) error
	}{
		{"dat-concordance-v1", func(w io.Writer, p Profile) error { return WriteDAT(w, records, p) }},
		{"csv-rfc4180-v1", func(w io.Writer, p Profile) error { return WriteCSV(w, records, p) }},
		{"opt-standard-v1", func(w io.Writer, p Profile) error { return WriteOPT(w, images, p) }},
	} {
		t.Run(tc.id, func(t *testing.T) {
			profile, err := ReadProfile(tc.id)
			require.NoError(t, err)
			file, err := os.CreateTemp(t.TempDir(), "closed-output")
			require.NoError(t, err)
			require.NoError(t, file.Close())
			require.ErrorIs(t, tc.write(file, profile), os.ErrClosed)
			err = tc.write(nil, profile)
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrInvalidProfile)
		})
	}
}

func TestLoadfileWriterEncodingPreservesLeadingContent(t *testing.T) {
	for _, id := range []string{"dat-concordance-v1", "csv-rfc4180-v1"} {
		for _, encoding := range []string{"utf-8-bom", "utf-16le", "utf-16be", "windows-1252", "iso-8859-1"} {
			t.Run(id+"/"+encoding, func(t *testing.T) {
				profile, err := ReadProfile(id)
				require.NoError(t, err)
				profile.Encoding = encoding
				column, value := "\ufeffTITLE", "\ufefftext 🙂"
				if encoding == "windows-1252" || encoding == "iso-8859-1" {
					column, value = "TITLE", "café"
				}
				record := Record{ColumnOrder: []string{column}, Fields: []Field{{Column: column, Raw: value}}}
				write, parse := WriteDAT, ParseDAT
				if id == "csv-rfc4180-v1" {
					write, parse = WriteCSV, ParseCSV
				}
				var output bytes.Buffer
				require.NoError(t, write(&output, []Record{record}, profile))
				parsed, diagnostics, err := parse(&output, profile)
				require.NoError(t, err)
				assert.Empty(t, diagnostics)
				require.Len(t, parsed, 1)
				assert.Equal(t, record.Fields, parsed[0].Fields)
			})
		}
	}
}
