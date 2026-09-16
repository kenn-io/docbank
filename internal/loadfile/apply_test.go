package loadfile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyMappingPopulatesRecordAuthorityFromConfirmedOrdinals(t *testing.T) {
	id, parent, native := "loadfile.document.id", "loadfile.family.parent", "loadfile.file.native"
	zero, one, two := 0, 1, 2
	records := []Record{{Fields: []Field{
		{Column: "Control", Ordinal: 0, Raw: "DOC-A"},
		{Column: "Parent", Ordinal: 1, Raw: "DOC-P"},
		{Column: "Native", Ordinal: 2, Raw: `VOL002\NATIVES\A.pdf`},
	}}}
	mapping := Mapping{Contract: MappingContractV1, DefaultCustodian: "Synthetic Custodian", Columns: []MappingColumn{
		{Source: "Control", SourceOrdinal: &zero, Canonical: &id},
		{Source: "Parent", SourceOrdinal: &one, Canonical: &parent},
		{Source: "Native", SourceOrdinal: &two, Canonical: &native},
	}}
	diagnostics, err := ApplyMapping(records, mapping, Profile{})
	require.NoError(t, err)
	NormalizeFileReferences(records, []Volume{{Name: "VOL002", DeclaredRoot: "VOL002"}})
	assert.Empty(t, diagnostics)
	assert.Equal(t, "DOC-A", records[0].DocID)
	assert.Equal(t, "DOC-P", records[0].Family.ParentDocID)
	require.Len(t, records[0].Files, 1)
	assert.Equal(t, "VOL002", records[0].Files[0].Volume)
	assert.Equal(t, "NATIVES/A.pdf", records[0].Files[0].RelPath)
	assert.Equal(t, "loadfile.custodian", records[0].Fields[len(records[0].Fields)-1].Canonical)
}

func TestNormalizeFileReferencesUsesDeclaredVolumeNames(t *testing.T) {
	records := []Record{{Files: []FileRef{
		{RelPath: "DISC001/native/a.pdf"},
		{RelPath: "native/b.pdf"},
	}}}
	NormalizeFileReferences(records, []Volume{{Name: "DISC001", DeclaredRoot: "DISC001"}})
	assert.Equal(t, "DISC001", records[0].Files[0].Volume)
	assert.Equal(t, "native/a.pdf", records[0].Files[0].RelPath)
	assert.Equal(t, "DISC001", records[0].Files[1].Volume)
	assert.Equal(t, "native/b.pdf", records[0].Files[1].RelPath)
}

func TestApplyMappingCombinesExplicitlyPairedDateAndTime(t *testing.T) {
	dateKey, timeKey := "loadfile.date.sent", "loadfile.time.sent"
	zero, one := 0, 1
	records := []Record{{LoadFile: "synthetic.dat", RowOrdinal: 2, Fields: []Field{
		{Column: "DATESENT", Ordinal: 0, Raw: "2026-09-12"},
		{Column: "TIMESENT", Ordinal: 1, Raw: "12:34:56Z"},
	}}}
	diagnostics, err := ApplyMapping(records, Mapping{Columns: []MappingColumn{
		{SourceOrdinal: &zero, Canonical: &dateKey},
		{SourceOrdinal: &one, Canonical: &timeKey, PairedDateOrdinal: &zero},
	}}, Profile{DateFormat: "YYYY-MM-DD"})
	require.NoError(t, err)
	require.Empty(t, diagnostics)
	require.NotNil(t, records[0].Fields[1].Value.Time)
	assert.Equal(t, "2026-09-12T12:34:56.000000000Z", *records[0].Fields[1].Value.Time.UTCKey)
	assert.Equal(t, "12:34:56Z", records[0].Fields[1].Raw)
}

func TestApplyMappingBoundsDateDiagnostics(t *testing.T) {
	key := "loadfile.date.sent"
	diagnostics := make([]Diagnostic, maxDiagnosticsPerOperation)
	_, err := mappedValue("ambiguous", MappingColumn{Canonical: &key}, Profile{}, &diagnostics)
	require.ErrorIs(t, err, ErrLoadfileLimit)
}

func TestApplyMappingHydratesConventionalColumnsWithDefaultCustodian(t *testing.T) {
	profile, err := ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	records := []Record{{LoadFile: "package.dat", RowOrdinal: 1, Fields: []Field{{Column: "DOCID", Raw: "DOC-A"}, {Column: "NATIVE", Raw: "NATIVES/a.pdf"}}}}
	_, err = ApplyMapping(records, Mapping{Contract: MappingContractV1, DefaultCustodian: "synthetic"}, profile)
	require.NoError(t, err)
	assert.Equal(t, "DOC-A", records[0].DocID)
	require.Len(t, records[0].Files, 1)
	assert.Equal(t, "NATIVES/a.pdf", records[0].Files[0].RelPath)
	assert.Equal(t, "loadfile.custodian", records[0].Fields[2].Canonical)
	assert.Equal(t, "synthetic", records[0].Fields[2].Raw)
	assert.NotEmpty(t, records[0].RowID)
}
