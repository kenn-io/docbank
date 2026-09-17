package loadfile

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func TestMappingRejectsMalformedOutOfCatalogAndOversizeDocuments(t *testing.T) {
	columns := []string{"BEGBATES", "CUSTODIAN"}
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "unknown member", raw: []byte(`{"contract":"loadfile-mapping/v1","columns":[],"nope":1}`)},
		{name: "unknown column member", raw: []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"BEGBATES","canonical":null,"nope":1}]}`)},
		{name: "missing source", raw: []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"MISSING","canonical":null}]}`)},
		{name: "foreign canonical namespace", raw: []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"BEGBATES","canonical":"email.sent"}]}`)},
		{name: "unknown loadfile key", raw: []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"BEGBATES","canonical":"loadfile.not.a.key"}]}`)},
		{name: "wrong contract", raw: []byte(`{"contract":"loadfile-mapping/v2","columns":[]}`)},
		{name: "malformed", raw: []byte(`{"contract":`)},
		{name: "duplicate member", raw: []byte(`{"contract":"loadfile-mapping/v1","contract":"loadfile-mapping/v1","columns":[]}`)},
		{name: "oversize", raw: []byte(strings.Repeat(" ", MaxMappingBytes+1))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := DecodeMapping(test.raw, columns)
			require.ErrorIs(t, err, ErrInvalidMapping)
		})
	}

	good := []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"BEGBATES","canonical":"loadfile.label.begin"}],"custodian_column":"CUSTODIAN"}`)
	mapping, digest, err := DecodeMapping(good, columns)
	require.NoError(t, err)
	assert.Equal(t, "CUSTODIAN", mapping.CustodianColumn)
	assert.Equal(t, 0, *mapping.Columns[0].SourceOrdinal)
	assert.True(t, canonical.IsSHA256Hex(digest))
}

func TestMappingPinsSourceAndPairedOrdinalsWithoutAmbiguity(t *testing.T) {
	columns := []string{"DATE", "TIME", "TEXT", "TEXT"}
	tests := []struct {
		name string
		raw  string
	}{
		{name: "duplicate source needs ordinal", raw: `{"contract":"loadfile-mapping/v1","columns":[{"source":"TEXT","canonical":null}]}`},
		{name: "source ordinal negative", raw: `{"contract":"loadfile-mapping/v1","columns":[{"source":"DATE","source_ordinal":-1,"canonical":null}]}`},
		{name: "source ordinal out of range", raw: `{"contract":"loadfile-mapping/v1","columns":[{"source":"DATE","source_ordinal":4,"canonical":null}]}`},
		{name: "source ordinal mismatch", raw: `{"contract":"loadfile-mapping/v1","columns":[{"source":"DATE","source_ordinal":1,"canonical":null}]}`},
		{name: "source ordinal claimed twice", raw: `{"contract":"loadfile-mapping/v1","columns":[{"source":"DATE","source_ordinal":0,"canonical":"loadfile.date.sent"},{"source":"DATE","source_ordinal":0,"canonical":"loadfile.date.received"}]}`},
		{name: "paired ordinal negative", raw: `{"contract":"loadfile-mapping/v1","columns":[{"source":"TIME","canonical":"loadfile.time.sent","paired_date_ordinal":-1}]}`},
		{name: "paired ordinal out of range", raw: `{"contract":"loadfile-mapping/v1","columns":[{"source":"TIME","canonical":"loadfile.time.sent","paired_date_ordinal":4}]}`},
		{name: "self pairing", raw: `{"contract":"loadfile-mapping/v1","columns":[{"source":"TIME","canonical":"loadfile.time.sent","paired_date_ordinal":1}]}`},
		{name: "unmapped date", raw: `{"contract":"loadfile-mapping/v1","columns":[{"source":"TIME","canonical":"loadfile.time.sent","paired_date_ordinal":0}]}`},
		{name: "non-date pairing", raw: `{"contract":"loadfile-mapping/v1","columns":[{"source":"DATE","canonical":"loadfile.document.name"},{"source":"TIME","canonical":"loadfile.time.sent","paired_date_ordinal":0}]}`},
		{name: "non-time pairing", raw: `{"contract":"loadfile-mapping/v1","columns":[{"source":"DATE","canonical":"loadfile.date.sent"},{"source":"TIME","canonical":"loadfile.document.name","paired_date_ordinal":0}]}`},
		{name: "unknown date format", raw: `{"contract":"loadfile-mapping/v1","columns":[{"source":"DATE","canonical":"loadfile.date.sent","date_format":"YY-DD-MM"}]}`},
		{name: "invalid timezone", raw: `{"contract":"loadfile-mapping/v1","columns":[{"source":"TIME","canonical":"loadfile.time.sent","timezone":"+99:00"}]}`},
		{name: "ambiguous custodian column", raw: `{"contract":"loadfile-mapping/v1","columns":[],"custodian_column":"TEXT"}`},
		{name: "missing custodian column", raw: `{"contract":"loadfile-mapping/v1","columns":[],"custodian_column":"MISSING"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := DecodeMapping([]byte(test.raw), columns)
			require.ErrorIs(t, err, ErrInvalidMapping)
		})
	}

	raw := []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"DATE","canonical":"loadfile.date.sent"},{"source":"TIME","canonical":"loadfile.time.sent","paired_date_ordinal":0},{"source":"TEXT","source_ordinal":3,"canonical":null}]}`)
	mapping, _, err := DecodeMapping(raw, columns)
	require.NoError(t, err)
	assert.Equal(t, 0, *mapping.Columns[0].SourceOrdinal)
	assert.Equal(t, 1, *mapping.Columns[1].SourceOrdinal)
	assert.Equal(t, 0, *mapping.Columns[1].PairedDateOrdinal)
	assert.Equal(t, 3, *mapping.Columns[2].SourceOrdinal)
}

func TestMappingRejectsDuplicateSingleValueTargets(t *testing.T) {
	for _, target := range []string{"loadfile.document.id", "loadfile.family.parent", "loadfile.family.id"} {
		t.Run(target, func(t *testing.T) {
			raw := `{"contract":"loadfile-mapping/v1","columns":[{"source":"A","canonical":"` + target + `"},{"source":"B","canonical":"` + target + `"}]}`
			_, _, err := DecodeMapping([]byte(raw), []string{"A", "B"})
			require.ErrorIs(t, err, ErrMappingAmbiguous)
		})
	}
	for _, target := range []string{"loadfile.file.native", "loadfile.file.produced_pdf", "loadfile.file.supplied_text", "loadfile.family.children", "loadfile.actor.recipient"} {
		t.Run(target, func(t *testing.T) {
			raw := `{"contract":"loadfile-mapping/v1","columns":[{"source":"A","canonical":"` + target + `"},{"source":"B","canonical":"` + target + `"}]}`
			_, _, err := DecodeMapping([]byte(raw), []string{"A", "B"})
			require.NoError(t, err)
		})
	}
}

func TestMappingRejectsNonPortableVolumeRoots(t *testing.T) {
	badRoots := []string{
		"/srv/production",
		`C:\production`,
		`\\server\share`,
		"../outside",
		"inside/../../outside",
		"inside\u0000outside",
		"VOL001:file",
		"production./native",
		"production /native",
	}
	for _, root := range badRoots {
		t.Run(root, func(t *testing.T) {
			raw := []byte(`{"contract":"loadfile-mapping/v1","columns":[],"volume_roots":{"VOL001":` + quoteJSON(root) + `}}`)
			_, _, err := DecodeMapping(raw, nil)
			require.ErrorIs(t, err, ErrInvalidMapping)
		})
	}

	for _, root := range []string{"VOL001", "production/native", `production\native`} {
		t.Run(root, func(t *testing.T) {
			raw := []byte(`{"contract":"loadfile-mapping/v1","columns":[],"volume_roots":{"VOL001":` + quoteJSON(root) + `}}`)
			mapping, _, err := DecodeMapping(raw, nil)
			require.NoError(t, err)
			assert.Equal(t, root, mapping.VolumeRoots["VOL001"])
		})
	}
}

func TestMappingDigestUsesTheCanonicalInterpretedMapping(t *testing.T) {
	columns := []string{"BEGBATES"}
	inferred := []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"BEGBATES","canonical":"loadfile.label.begin"}],"volume_roots":{"B":"b","A":"a"}}`)
	explicit := []byte(`{ "volume_roots": {"A":"a", "B":"b"}, "columns": [{"canonical":"loadfile.label.begin", "source_ordinal":0, "source":"BEGBATES"}], "contract":"loadfile-mapping/v1" }`)

	_, first, err := DecodeMapping(inferred, columns)
	require.NoError(t, err)
	_, second, err := DecodeMapping(explicit, columns)
	require.NoError(t, err)
	assert.Equal(t, first, second)

	changed := []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"BEGBATES","canonical":"loadfile.label.begin","sensitive":true}],"volume_roots":{"B":"b","A":"a"}}`)
	_, third, err := DecodeMapping(changed, columns)
	require.NoError(t, err)
	assert.NotEqual(t, first, third)
}

func quoteJSON(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\x00", `\u0000`)
	return `"` + replacer.Replace(value) + `"`
}
