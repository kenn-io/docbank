package loadfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManifestBindsSameSizeSourceReplacement(t *testing.T) {
	m := Manifest{Files: []FileRef{{
		Role: "raw_load_file", RelPath: "loadfiles/production.dat",
		SHA256: strings.Repeat("a", 64), Size: 5, Status: "available",
	}}}
	before, err := m.SHA256()
	require.NoError(t, err)
	m.Files[0].SHA256 = strings.Repeat("b", 64)
	after, err := m.SHA256()
	require.NoError(t, err)
	require.NotEqual(t, before, after)
}

func TestManifestDigestIsStableAndBindsPortableAuthority(t *testing.T) {
	offset := 3600
	utcKey := "2026-09-12T11:34:56.123000000Z"
	m := Manifest{
		ProfileSHA256: strings.Repeat("a", 64),
		MappingSHA256: strings.Repeat("b", 64),
		Volumes:       []Volume{{Name: "VOL001", DeclaredRoot: "sender/VOL001", Ordinal: 1}},
		Records: []Record{{
			RowID: "row-a", DocID: "DOC0001", LoadFile: "loadfiles/production.dat", RowOrdinal: 2,
			ColumnOrder: []string{"DATESENT", "DATESENT"},
			Fields: []Field{{
				Column: "DATESENT", Ordinal: 1, Canonical: "loadfile.date.sent", Raw: "09/12/2026 12:34:56.123 +0100",
				Value: Value{Kind: "time", Time: &TimeClaim{
					Raw: "09/12/2026 12:34:56.123 +0100", DateValue: "2026-09-12T12:34:56.123",
					Precision: "fraction", TimezoneKind: "offset", ZoneText: "+0100",
					OffsetSeconds: &offset, UTCKey: &utcKey,
				}},
			}},
			Files:  []FileRef{{Role: "native", Volume: "VOL001", RelPath: "native/a.msg", SHA256: strings.Repeat("c", 64), Size: 17, Status: "available"}},
			Family: Family{ParentDocID: "PARENT", GroupID: "family-1"},
		}},
		Images: []ImageRef{{ImageKey: "DOC0001", Volume: "VOL001", RelPath: "images/a.tif", DocumentBreak: true, PageOrdinal: 1, SourcePage: 1}},
		Files:  []FileRef{{Role: "raw_load_file", RelPath: "loadfiles/production.dat", SHA256: strings.Repeat("d", 64), Size: 91, Status: "available"}},
	}

	first, err := m.SHA256()
	require.NoError(t, err)
	second, err := m.SHA256()
	require.NoError(t, err)
	assert.Equal(t, first, second)

	mutations := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{name: "profile", mutate: func(m *Manifest) { m.ProfileSHA256 = strings.Repeat("e", 64) }},
		{name: "mapping", mutate: func(m *Manifest) { m.MappingSHA256 = strings.Repeat("e", 64) }},
		{name: "volume", mutate: func(m *Manifest) { m.Volumes[0].DeclaredRoot = "sender/VOL002" }},
		{name: "record order", mutate: func(m *Manifest) { m.Records = append([]Record{{RowID: "prefix"}}, m.Records...) }},
		{name: "field order", mutate: func(m *Manifest) { m.Records[0].Fields[0].Ordinal = 0 }},
		{name: "sender time", mutate: func(m *Manifest) { m.Records[0].Fields[0].Value.Time.Raw = "changed" }},
		{name: "record file", mutate: func(m *Manifest) { m.Records[0].Files[0].Size++ }},
		{name: "family", mutate: func(m *Manifest) { m.Records[0].Family.ParentDocID = "OTHER" }},
		{name: "image", mutate: func(m *Manifest) { m.Images[0].SourcePage = 2 }},
		{name: "package file", mutate: func(m *Manifest) { m.Files[0].Status = "missing" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := cloneManifestForTest(m)
			mutation.mutate(&changed)
			digest, err := changed.SHA256()
			require.NoError(t, err)
			assert.NotEqual(t, first, digest)
		})
	}
}

func TestManifestWritesCanonicalJSONLIncrementallyInDeclaredOrder(t *testing.T) {
	m := Manifest{
		ProfileSHA256: "profile",
		MappingSHA256: "mapping",
		Volumes: []Volume{
			{Name: "VOL002", DeclaredRoot: "second", Ordinal: 2},
			{Name: "VOL001", DeclaredRoot: "first", Ordinal: 1},
		},
		Records: []Record{{RowID: "second"}, {RowID: "first"}},
		Images:  []ImageRef{{ImageKey: "page-2"}, {ImageKey: "page-1"}},
		Files:   []FileRef{{RelPath: "z.dat", Size: 2}, {RelPath: "a.dat", Size: 1}},
	}
	want := "" +
		`{"kind":"manifest","mapping_sha256":"mapping","profile_sha256":"profile"}` + "\n" +
		`{"kind":"volume","value":{"declared_root":"second","name":"VOL002","ordinal":2}}` + "\n" +
		`{"kind":"volume","value":{"declared_root":"first","name":"VOL001","ordinal":1}}` + "\n" +
		`{"kind":"record","value":{"column_order":[],"doc_id":"","family":{"attachment_doc_ids":[],"group_id":"","parent_doc_id":"","range_begin":"","range_end":""},"fields":[],"files":[],"load_file":"","row_id":"second","row_ordinal":0}}` + "\n" +
		`{"kind":"record","value":{"column_order":[],"doc_id":"","family":{"attachment_doc_ids":[],"group_id":"","parent_doc_id":"","range_begin":"","range_end":""},"fields":[],"files":[],"load_file":"","row_id":"first","row_ordinal":0}}` + "\n" +
		`{"kind":"image","value":{"boundary":"","box_break":false,"declared_page_count":0,"document_break":false,"folder_break":false,"image_key":"page-2","page_ordinal":0,"rel_path":"","rotation":0,"source_page":0,"volume":""}}` + "\n" +
		`{"kind":"image","value":{"boundary":"","box_break":false,"declared_page_count":0,"document_break":false,"folder_break":false,"image_key":"page-1","page_ordinal":0,"rel_path":"","rotation":0,"source_page":0,"volume":""}}` + "\n" +
		`{"kind":"file","value":{"declared":"","rel_path":"z.dat","role":"","sha256":"","size":2,"status":"","volume":""}}` + "\n" +
		`{"kind":"file","value":{"declared":"","rel_path":"a.dat","role":"","sha256":"","size":1,"status":"","volume":""}}` + "\n"

	var output bytes.Buffer
	require.NoError(t, writeManifestJSONL(&output, m))
	assert.Equal(t, want, output.String())

	wantDigest := sha256.Sum256([]byte(want))
	digest, err := m.SHA256()
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(wantDigest[:]), digest)
}

func cloneManifestForTest(source Manifest) Manifest {
	clone := source
	clone.Volumes = append([]Volume(nil), source.Volumes...)
	clone.Images = append([]ImageRef(nil), source.Images...)
	clone.Files = append([]FileRef(nil), source.Files...)
	clone.Records = append([]Record(nil), source.Records...)
	clone.Records[0].ColumnOrder = append([]string(nil), source.Records[0].ColumnOrder...)
	clone.Records[0].Fields = append([]Field(nil), source.Records[0].Fields...)
	clone.Records[0].Files = append([]FileRef(nil), source.Records[0].Files...)
	claim := *source.Records[0].Fields[0].Value.Time
	clone.Records[0].Fields[0].Value.Time = &claim
	return clone
}
