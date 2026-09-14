package loadfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/encoding/charmap"
)

func TestLoadfileCollectorNeverAllocatesTheNextPage(t *testing.T) {
	rows, _, err := collectRecords(func(emit func(Record) error) ([]Diagnostic, error) {
		for i := 0; i <= MaxRowsPerPage; i++ {
			if err := emit(Record{RowOrdinal: i + 1}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	require.ErrorIs(t, err, ErrLoadfileLimit)
	require.Len(t, rows, MaxRowsPerPage)
}

func TestMalformedInputDoesNotBlameTheProfile(t *testing.T) {
	for _, tc := range []struct {
		name, id, raw string
		parse         func(io.Reader, Profile) ([]Record, []Diagnostic, error)
	}{
		{"unterminated DAT", "dat-concordance-v1", "þunfinished", ParseDAT},
		{"invalid encoding", "dat-concordance-v1", "\xff", ParseDAT},
		{"oversized field", "dat-concordance-v1", strings.Repeat("x", MaxFieldValueBytes+1), ParseDAT},
		{"malformed CSV", "csv-rfc4180-v1", "bare\"quote\n", ParseCSV},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := tc.parse(strings.NewReader(tc.raw), mustProfile(t, tc.id))
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrInvalidProfile)
			require.ErrorIs(t, err, ErrMalformedInput)
		})
	}
}

func TestLoadfileScannersPreserveRecordsAcrossLineEndingsAndBlankRows(t *testing.T) {
	for _, tc := range []struct {
		id, separator, qualifier string
		parse                    func(io.Reader, Profile) ([]Record, []Diagnostic, error)
	}{
		{"dat-concordance-v1", "\x14", "þ", ParseDAT},
		{"csv-rfc4180-v1", ",", "\"", ParseCSV},
	} {
		for _, newline := range []string{"\n", "\r\n", "\r"} {
			for _, qualifier := range []string{"", tc.qualifier} {
				t.Run(fmt.Sprintf("%s/%q/%q", tc.id, newline, qualifier), func(t *testing.T) {
					row := qualifier + "001" + qualifier + tc.separator + qualifier + "value" + qualifier + newline
					raw := "ID" + tc.separator + "VALUE" + newline + row + newline + row + newline
					records, diagnostics, err := tc.parse(strings.NewReader(raw), mustProfile(t, tc.id))
					require.NoError(t, err)
					require.Len(t, records, 4)
					assert.Equal(t, "001", records[0].Fields[0].Raw)
					assert.Equal(t, "value", records[2].Fields[1].Raw)
					require.Len(t, diagnostics, 2)
					assert.Equal(t, "blocking", diagnostics[0].Severity)
					assert.Equal(t, 3, diagnostics[0].RowOrdinal)
					assert.Equal(t, 5, diagnostics[1].RowOrdinal)
				})
			}
		}
	}
}

func TestLoadfileHeaderMustMatchDeclaredColumns(t *testing.T) {
	for _, tc := range []struct {
		id, raw string
		parse   func(io.Reader, Profile) ([]Record, []Diagnostic, error)
	}{
		{"dat-concordance-v1", "ACTUAL\n001\n", ParseDAT},
		{"csv-rfc4180-v1", "ACTUAL\n001\n", ParseCSV},
	} {
		t.Run(tc.id, func(t *testing.T) {
			p := mustProfile(t, tc.id)
			p.Columns = []string{"DECLARED"}
			rows, _, err := tc.parse(strings.NewReader(tc.raw), p)
			require.Error(t, err)
			assert.Empty(t, rows)
			p.Columns = []string{"ACTUAL"}
			rows, diagnostics, err := tc.parse(strings.NewReader(tc.raw), p)
			require.NoError(t, err)
			assert.Empty(t, diagnostics)
			require.Len(t, rows, 1)
			assert.Equal(t, "ACTUAL", rows[0].Fields[0].Column)
		})
	}
}

func TestDATScannerHonoursQualifiersMultilineAndLeadingZeros(t *testing.T) {
	raw := readLoadfileFixture(t, "ab-package.dat")
	p := mustProfile(t, "dat-concordance-v1")
	p.Encoding = "windows-1252"

	records, diagnostics, err := ParseDAT(bytes.NewReader(raw), p)
	require.NoError(t, err)
	assert.Empty(t, diagnostics)
	require.Len(t, records, 2)
	assert.Equal(t, 2, records[0].RowOrdinal)
	assert.Equal(t, 3, records[1].RowOrdinal)
	assert.Equal(t, []string{"BEGBATES", "CUSTODIAN", "TEXT", "UNKNOWN", "TEXT"}, records[0].ColumnOrder)
	assert.Equal(t, "EXT000001", records[0].Fields[0].Raw)
	assert.Equal(t, "line one\nline two", records[0].Fields[2].Raw)
	assert.Equal(t, "00123", records[0].Fields[3].Raw)
	assert.Equal(t, "a þb", records[1].Fields[2].Raw)
	assert.Equal(t, "00007", records[1].Fields[3].Raw)
	assert.Equal(t, "€ alpha", records[0].Fields[4].Raw)
	assert.Equal(t, []int{0, 1, 2, 3, 4}, fieldOrdinals(records[0]))

	p.Encoding = "iso-8859-1"
	latinRecords, _, err := ParseDAT(bytes.NewReader(raw), p)
	require.NoError(t, err)
	assert.Equal(t, "\u0080 alpha", latinRecords[0].Fields[4].Raw)

	p.Encoding = "utf-8"
	_, _, err = ParseDAT(bytes.NewReader(raw), p)
	require.ErrorIs(t, err, ErrMalformedInput)

	utf8Raw, err := charmap.Windows1252.NewDecoder().Bytes(raw)
	require.NoError(t, err)
	utf8Records, _, err := ParseDAT(bytes.NewReader(utf8Raw), p)
	require.NoError(t, err)
	assert.Equal(t, "a þb", utf8Records[1].Fields[2].Raw)
}

func TestCSVScannerHonoursRFC4180MultilineDuplicatesAndLeadingZeros(t *testing.T) {
	raw := readLoadfileFixture(t, "ab-package.csv")
	p := mustProfile(t, "csv-rfc4180-v1")

	records, diagnostics, err := ParseCSV(bytes.NewReader(raw), p)
	require.NoError(t, err)
	assert.Empty(t, diagnostics)
	require.Len(t, records, 2)
	assert.Equal(t, 2, records[0].RowOrdinal)
	assert.Equal(t, []string{"BEGBATES", "CUSTODIAN", "TEXT", "UNKNOWN", "TEXT"}, records[0].ColumnOrder)
	assert.Equal(t, "line one\nline two", records[0].Fields[2].Raw)
	assert.Equal(t, "00123", records[0].Fields[3].Raw)
	assert.Equal(t, "a \"b\"", records[1].Fields[2].Raw)
	assert.Equal(t, "00007", records[1].Fields[3].Raw)
	assert.Equal(t, []int{0, 1, 2, 3, 4}, fieldOrdinals(records[0]))
}

func TestLoadfileScannersRejectEncodingAndGrammarDamage(t *testing.T) {
	datProfile := mustProfile(t, "dat-concordance-v1")
	datProfile.Encoding = "windows-1252"
	datFixture := readLoadfileFixture(t, "ab-package.dat")

	_, _, err := ParseDAT(bytes.NewReader(append([]byte{0xef, 0xbb, 0xbf}, datFixture...)), datProfile)
	require.ErrorIs(t, err, ErrMalformedInput)

	datProfile.Encoding = "utf-8"
	utf8Fixture, err := charmap.Windows1252.NewDecoder().Bytes(datFixture)
	require.NoError(t, err)
	mixed := append([]byte(nil), utf8Fixture...)
	mixed[len(mixed)-2] = 0x80
	_, _, err = ParseDAT(bytes.NewReader(mixed), datProfile)
	require.ErrorIs(t, err, ErrMalformedInput)

	const q, sep = "þ", "\x14"
	malformedDAT := q + "A" + q + "x" + sep + q + "B" + q + "\n"
	_, _, err = ParseDAT(strings.NewReader(malformedDAT), datProfile)
	require.ErrorIs(t, err, ErrMalformedInput)

	csvProfile := mustProfile(t, "csv-rfc4180-v1")
	_, _, err = ParseCSV(strings.NewReader("A,B\r\n1,un\"qualified\r\n"), csvProfile)
	require.ErrorIs(t, err, ErrMalformedInput)
}

func TestLoadfileScannersEnforceColumnFieldAndRecordBounds(t *testing.T) {
	t.Run("DAT columns", func(t *testing.T) {
		p := headerlessProfile(t, "dat-concordance-v1", MaxColumnsPerRow)
		_, _, err := ParseDAT(strings.NewReader(strings.Repeat("x\x14", 512)+"x\n"), p)
		require.ErrorIs(t, err, ErrMalformedInput)
	})
	t.Run("CSV columns", func(t *testing.T) {
		p := headerlessProfile(t, "csv-rfc4180-v1", MaxColumnsPerRow)
		_, _, err := ParseCSV(strings.NewReader(strings.Repeat("x,", 512)+"x\r\n"), p)
		require.ErrorIs(t, err, ErrMalformedInput)
	})
	t.Run("CSV decoded field", func(t *testing.T) {
		p := headerlessProfile(t, "csv-rfc4180-v1", 1)
		_, _, err := ParseCSV(strings.NewReader(strings.Repeat("x", MaxFieldValueBytes+1)+"\r\n"), p)
		require.ErrorIs(t, err, ErrMalformedInput)
	})
	t.Run("DAT normalized record", func(t *testing.T) {
		p := headerlessProfile(t, "dat-concordance-v1", 17)
		field := strings.Repeat("x", MaxFieldValueBytes)
		raw := strings.Repeat(field+"\x14", 16) + field + "\n"
		_, _, err := ParseDAT(strings.NewReader(raw), p)
		require.ErrorIs(t, err, ErrMalformedInput)
	})
	t.Run("CSV normalized record", func(t *testing.T) {
		p := headerlessProfile(t, "csv-rfc4180-v1", 17)
		field := strings.Repeat("x", MaxFieldValueBytes)
		raw := strings.Repeat(field+",", 16) + field + "\r\n"
		_, _, err := ParseCSV(strings.NewReader(raw), p)
		require.ErrorIs(t, err, ErrMalformedInput)
	})
	t.Run("CSV raw quoted multiline row", func(t *testing.T) {
		p := headerlessProfile(t, "csv-rfc4180-v1", 1)
		raw := "\"" + strings.Repeat("x\n", maxCSVRawRowBytes/2) + "\"\r\n"
		_, _, err := ParseCSV(strings.NewReader(raw), p)
		require.ErrorIs(t, err, ErrMalformedInput)
	})
}

func TestDATScannerRejectsOversizedValue(t *testing.T) {
	p := headerlessProfile(t, "dat-concordance-v1", 1)
	raw := "þ" + strings.Repeat("x", MaxFieldValueBytes+1) + "þ\n"
	_, _, err := ParseDAT(strings.NewReader(raw), p)
	require.ErrorIs(t, err, ErrMalformedInput)
}

func TestCSVScannerEnforcesDecodedHeaderBounds(t *testing.T) {
	p := mustProfile(t, "csv-rfc4180-v1")
	_, _, err := ParseCSV(strings.NewReader(strings.Repeat("A,", MaxColumnsPerRow)+"A\r\n"), p)
	require.ErrorIs(t, err, ErrMalformedInput)

	_, _, err = ParseCSV(strings.NewReader(strings.Repeat("A", MaxFieldValueBytes+1)+"\r\n"), p)
	require.ErrorIs(t, err, ErrMalformedInput)
}

func TestLoadfileScannersRejectOversizedDeclaredHeadersBeforeReading(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		scan func(io.Reader, Profile, func(Record) error) ([]Diagnostic, error)
	}{
		{name: "DAT", id: "dat-concordance-v1", scan: ScanDAT},
		{name: "CSV", id: "csv-rfc4180-v1", scan: ScanCSV},
	} {
		for _, header := range []struct {
			name    string
			columns []string
		}{
			{name: "field", columns: []string{strings.Repeat("x", MaxFieldValueBytes+1)}},
			{name: "row", columns: makeRepeatedStrings(17, strings.Repeat("x", MaxFieldValueBytes))},
		} {
			t.Run(tc.name+" "+header.name, func(t *testing.T) {
				p := mustProfile(t, tc.id)
				p.HeaderRow = false
				p.Columns = header.columns
				source := &readCountingErrorReader{err: errors.New("source must not be read")}
				_, err := tc.scan(source, p, func(Record) error { return nil })
				require.ErrorIs(t, err, ErrInvalidProfile)
				assert.Zero(t, source.reads)
			})
		}
	}
}

func TestDATScannerMapsNewlineMarkerInBareFields(t *testing.T) {
	p := headerlessProfile(t, "dat-concordance-v1", 1)
	records, diagnostics, err := ParseDAT(strings.NewReader("®first\nmiddle®last\n"), p)
	require.NoError(t, err)
	assert.Empty(t, diagnostics)
	require.Len(t, records, 2)
	assert.Equal(t, "\nfirst", records[0].Fields[0].Raw)
	assert.Equal(t, "middle\nlast", records[1].Fields[0].Raw)
}

func TestDATScannerTreatsCRLFAsARowTerminatorOnlyOutsideQualifiedFields(t *testing.T) {
	p := headerlessProfile(t, "dat-concordance-v1", 2)
	for _, tc := range []struct {
		name, raw             string
		wantFirst, wantSecond string
	}{
		{name: "bare final field", raw: "A\x14B\r\n", wantFirst: "A", wantSecond: "B"},
		{name: "qualified final field", raw: "þAþ\x14þBþ\r\n", wantFirst: "A", wantSecond: "B"},
		{name: "qualified CRLF data", raw: "þA\r\nBþ\x14þCþ\r\n", wantFirst: "A\r\nB", wantSecond: "C"},
		{name: "qualified lone CR data", raw: "A\x14þB\rCþ\n", wantFirst: "A", wantSecond: "B\rC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records, diagnostics, err := ParseDAT(strings.NewReader(tc.raw), p)
			require.NoError(t, err)
			assert.Empty(t, diagnostics)
			require.Len(t, records, 1)
			require.Len(t, records[0].Fields, 2)
			assert.Equal(t, tc.wantFirst, records[0].Fields[0].Raw)
			assert.Equal(t, tc.wantSecond, records[0].Fields[1].Raw)
		})
	}

	t.Run("explicit CR field delimiter", func(t *testing.T) {
		declared := p
		declared.Field = '\r'
		declared.Qualifier = '"'
		declared.NewlineInField = 0
		records, diagnostics, err := ParseDAT(strings.NewReader("A\r\n"), declared)
		require.NoError(t, err)
		assert.Empty(t, diagnostics)
		require.Len(t, records, 1)
		require.Len(t, records[0].Fields, 2)
		assert.Equal(t, "A", records[0].Fields[0].Raw)
		assert.Empty(t, records[0].Fields[1].Raw)
	})
}

func TestDATScannerPreservesDeclaredCRQualifierPrecedenceInBareFields(t *testing.T) {
	p := headerlessProfile(t, "dat-concordance-v1", 1)
	p.Qualifier = '\r'
	p.NewlineInField = 0

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "CRLF", raw: "bare\r\n"},
		{name: "lone CR", raw: "bare\rrest\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseDAT(strings.NewReader(tc.raw), p)
			require.ErrorIs(t, err, ErrMalformedInput)
			assert.ErrorContains(t, err, "qualifier in bare DAT field")
		})
	}
}

func TestLoadfileScannersRequireColumnsAndDiagnoseMismatches(t *testing.T) {
	for _, tc := range []struct {
		name string
		scan func(io.Reader, Profile, func(Record) error) ([]Diagnostic, error)
		id   string
		raw  string
	}{
		{name: "DAT", scan: ScanDAT, id: "dat-concordance-v1", raw: "1\x142\n"},
		{name: "CSV", scan: ScanCSV, id: "csv-rfc4180-v1", raw: "1,2\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mustProfile(t, tc.id)
			p.HeaderRow = false
			p.Columns = nil
			_, err := tc.scan(strings.NewReader(tc.raw), p, func(Record) error { return nil })
			require.ErrorIs(t, err, ErrInvalidProfile)

			p.Columns = []string{"A"}
			var got Record
			diagnostics, err := tc.scan(strings.NewReader(tc.raw), p, func(record Record) error {
				got = record
				return nil
			})
			require.NoError(t, err)
			require.Len(t, diagnostics, 1)
			assert.Equal(t, "column_count_mismatch", diagnostics[0].Code)
			assert.Equal(t, "blocking", diagnostics[0].Severity)
			assert.Equal(t, 1, diagnostics[0].RowOrdinal)
			assert.Equal(t, []string{"A"}, got.ColumnOrder)
			require.Len(t, got.Fields, 2)
			assert.Empty(t, got.Fields[1].Column)
		})
	}
}

func TestLoadfileScannersPropagateCallbackAndSourceErrorsImmediately(t *testing.T) {
	callbackErr := errors.New("callback stopped")
	sourceErr := errors.New("source stopped")
	for _, tc := range []struct {
		name string
		scan func(io.Reader, Profile, func(Record) error) ([]Diagnostic, error)
		id   string
		raw  string
	}{
		{name: "DAT", scan: ScanDAT, id: "dat-concordance-v1", raw: "A\n1\n2\n"},
		{name: "CSV", scan: ScanCSV, id: "csv-rfc4180-v1", raw: "A\r\n1\r\n2\r\n"},
	} {
		t.Run(tc.name+" callback", func(t *testing.T) {
			p := mustProfile(t, tc.id)
			calls := 0
			_, err := tc.scan(strings.NewReader(tc.raw), p, func(Record) error {
				calls++
				return callbackErr
			})
			require.ErrorIs(t, err, callbackErr)
			assert.Equal(t, 1, calls)
		})
		t.Run(tc.name+" source", func(t *testing.T) {
			p := mustProfile(t, tc.id)
			p.HeaderRow = false
			p.Columns = []string{"A"}
			reader := &errorAfterReader{data: []byte("partial"), err: sourceErr}
			calls := 0
			_, err := tc.scan(reader, p, func(Record) error {
				calls++
				return nil
			})
			require.ErrorIs(t, err, sourceErr)
			assert.Zero(t, calls)
		})
	}
}

func TestLoadfileParseCollectorsStopAt4096Records(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parse func(io.Reader, Profile) ([]Record, []Diagnostic, error)
		id    string
		head  string
		row   string
	}{
		{name: "DAT", parse: ParseDAT, id: "dat-concordance-v1", head: "A\n", row: "1\n"},
		{name: "CSV", parse: ParseCSV, id: "csv-rfc4180-v1", head: "A\r\n", row: "1\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mustProfile(t, tc.id)
			rows, _, err := tc.parse(strings.NewReader(tc.head+strings.Repeat(tc.row, MaxRowsPerPage+1)), p)
			require.ErrorIs(t, err, ErrLoadfileLimit)
			require.Len(t, rows, MaxRowsPerPage)
		})
	}
}

func TestLoadfileScannersStream100000Records(t *testing.T) {
	for _, tc := range []struct {
		name string
		scan func(io.Reader, Profile, func(Record) error) ([]Diagnostic, error)
		id   string
		head string
		row  string
	}{
		{name: "DAT", scan: ScanDAT, id: "dat-concordance-v1", head: "þIDþ\x14þVALUEþ\n", row: "þ000001þ\x14þvalueþ\n"},
		{name: "CSV", scan: ScanCSV, id: "csv-rfc4180-v1", head: "ID,VALUE\r\n", row: "000001,value\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mustProfile(t, tc.id)
			count := 0
			diagnostics, err := tc.scan(io.MultiReader(strings.NewReader(tc.head), strings.NewReader(strings.Repeat(tc.row, 100_000))), p, func(record Record) error {
				count++
				if count == 100_000 {
					assert.Equal(t, 100_001, record.RowOrdinal)
				}
				return nil
			})
			require.NoError(t, err)
			assert.Empty(t, diagnostics)
			assert.Equal(t, 100_000, count)
		})
	}
}

func readLoadfileFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return raw
}

func mustProfile(t *testing.T, id string) Profile {
	t.Helper()
	p, err := ReadProfile(id)
	require.NoError(t, err)
	return p
}

func headerlessProfile(t *testing.T, id string, columns int) Profile {
	t.Helper()
	p := mustProfile(t, id)
	p.HeaderRow = false
	p.Columns = make([]string, columns)
	for index := range p.Columns {
		p.Columns[index] = fmt.Sprintf("COLUMN_%03d", index)
	}
	return p
}

func fieldOrdinals(record Record) []int {
	ordinals := make([]int, len(record.Fields))
	for index := range record.Fields {
		ordinals[index] = record.Fields[index].Ordinal
	}
	return ordinals
}

func makeRepeatedStrings(count int, value string) []string {
	values := make([]string, count)
	for index := range values {
		values[index] = value
	}
	return values
}

type errorAfterReader struct {
	data []byte
	err  error
}

type readCountingErrorReader struct {
	reads int
	err   error
}

func (r *readCountingErrorReader) Read([]byte) (int, error) {
	r.reads++
	return 0, r.err
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}
