package document

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSourceMetadataV1CanonicalGolden(t *testing.T) {
	pageCount := int64(7)
	record := SourceMetadataV1{
		ContractVersion: SourceMetadataContractV1,
		Fields: []SourceMetadataFieldV1{
			{Key: "pdf.info.page_count", Namespace: "pdf.info", SourceField: "PageCount",
				Value: SourceMetadataValueV1{Kind: SourceMetadataInteger, Integer: &pageCount}},
			{Key: "creators", Namespace: "pdf.info", SourceField: "Author",
				Value: SourceMetadataValueV1{Kind: SourceMetadataStringList, Strings: []string{"Ada Lovelace", "Grace Hopper"}}},
			{Key: "created", Namespace: "xmp", SourceField: "CreateDate",
				Value: SourceMetadataValueV1{Kind: SourceMetadataTimestamp, Timestamp: &SourceMetadataTimestampV1{
					Raw: "2024-01-02T03:04:05-07:00", Normalized: "2024-01-02T03:04:05-07:00",
					Offset: "-07:00", Precision: SourceMetadataPrecisionSecond, Timezone: SourceMetadataTimezoneOffset,
				}}},
			{Key: "title", Namespace: "pdf.info", SourceField: "Title",
				Value: SourceMetadataValueV1{Kind: SourceMetadataString, String: new("Synthetic report")}},
		},
		Warnings: []SourceMetadataWarningV1{{
			Code: "unparseable_timestamp", Namespace: "pdf.info", SourceField: "ModDate",
			Detail: "value retained without coercion",
		}},
	}

	canonical, checksum, err := MarshalSourceMetadataV1(record)
	require.NoError(t, err)
	assert.Len(t, checksum, 64)
	want, err := os.ReadFile(filepath.Join("testdata", "source-metadata-v1.golden.json"))
	require.NoError(t, err)
	want = bytes.TrimSuffix(want, []byte("\n"))
	assert.Equal(t, want, canonical)

	decoded, decodedChecksum, err := DecodeSourceMetadataV1(canonical)
	require.NoError(t, err)
	assert.Equal(t, checksum, decodedChecksum)
	assert.Equal(t, []string{"created", "creators", "pdf.info.page_count", "title"},
		[]string{decoded.Fields[0].Key, decoded.Fields[1].Key, decoded.Fields[2].Key, decoded.Fields[3].Key})
}

func TestSourceMetadataV1RejectsAmbiguityAndUnboundedValues(t *testing.T) {
	base := SourceMetadataV1{ContractVersion: SourceMetadataContractV1,
		Fields: []SourceMetadataFieldV1{{Key: "title", Namespace: "pdf.info", SourceField: "Title",
			Value: SourceMetadataValueV1{Kind: SourceMetadataString, String: new("safe")}}}}
	for _, testCase := range []struct {
		name   string
		mutate func(*SourceMetadataV1)
		want   string
	}{
		{name: "duplicate canonical key", want: "duplicate canonical key", mutate: func(v *SourceMetadataV1) {
			v.Fields = append(v.Fields, v.Fields[0])
		}},
		{name: "invalid UTF-8", want: "UTF-8", mutate: func(v *SourceMetadataV1) {
			v.Fields[0].Value.String = new(string([]byte{0xff}))
		}},
		{name: "oversized value", want: "too large", mutate: func(v *SourceMetadataV1) {
			v.Fields[0].Value.String = new(string(make([]byte, MaxSourceMetadataValueBytes+1)))
		}},
		{name: "oversized key", want: "forbidden canonical key", mutate: func(v *SourceMetadataV1) {
			v.Fields[0].Key = "office.custom." + strings.Repeat("a", MaxSourceMetadataLabelBytes)
		}},
		{name: "unbounded array", want: "too many", mutate: func(v *SourceMetadataV1) {
			v.Fields[0].Value = SourceMetadataValueV1{Kind: SourceMetadataStringList,
				Strings: make([]string, MaxSourceMetadataListValues+1)}
		}},
		{name: "non-finite number", want: "finite", mutate: func(v *SourceMetadataV1) {
			number := math.Inf(1)
			v.Fields[0].Value = SourceMetadataValueV1{Kind: SourceMetadataNumber, Number: &number}
		}},
		{name: "ambiguous timestamp", want: "timezone", mutate: func(v *SourceMetadataV1) {
			v.Fields[0].Value = SourceMetadataValueV1{Kind: SourceMetadataTimestamp,
				Timestamp: &SourceMetadataTimestampV1{Raw: "01/02/03", Normalized: "2003-01-02",
					Precision: SourceMetadataPrecisionDate, Timezone: SourceMetadataTimezoneOffset}}
		}},
		{name: "unknown major", want: "contract version", mutate: func(v *SourceMetadataV1) {
			v.ContractVersion = "source-metadata/v2"
		}},
		{name: "unknown namespace", want: "unknown namespace", mutate: func(v *SourceMetadataV1) {
			v.Fields[0].Namespace = "provider.guess"
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			value := base
			value.Fields = append([]SourceMetadataFieldV1(nil), base.Fields...)
			testCase.mutate(&value)
			_, _, err := MarshalSourceMetadataV1(value)
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

func TestSourceMetadataV1KeepsAttachmentFactsOutOfContentRecord(t *testing.T) {
	for _, key := range []string{
		"filename", "extension", "source_path", "ingest_time", "filesystem_mtime",
		"bates_start", "bates_end", "production_volume", "custodian", "redaction_state",
		"family_date", "collection_processing_timezone", "duplicate_path", "produced_document_link",
	} {
		assert.False(t, SourceMetadataCanonicalKeyAllowed(key), key)
	}
	assert.True(t, SourceMetadataCanonicalKeyAllowed("title"))
	assert.True(t, SourceMetadataCanonicalKeyAllowed("image.exif.gps_latitude"))
}

func TestSourceMetadataV1PreservesTimestampPrecisionAndTimezoneEvidence(t *testing.T) {
	for _, stamp := range []SourceMetadataTimestampV1{
		{Raw: "2024-01-02", Normalized: "2024-01-02", Precision: SourceMetadataPrecisionDate, Timezone: SourceMetadataTimezoneOmitted},
		{Raw: "2024-01-02T03:04:05Z", Normalized: "2024-01-02T03:04:05Z", Precision: SourceMetadataPrecisionSecond, Timezone: SourceMetadataTimezoneUTC},
		{Raw: "2024-01-02T03:04:05.123-07:00", Normalized: "2024-01-02T03:04:05.123-07:00", Offset: "-07:00", Precision: SourceMetadataPrecisionFraction, Timezone: SourceMetadataTimezoneOffset},
	} {
		record := SourceMetadataV1{ContractVersion: SourceMetadataContractV1, Fields: []SourceMetadataFieldV1{{Key: "created", Namespace: "xmp", SourceField: "CreateDate", Value: SourceMetadataValueV1{Kind: SourceMetadataTimestamp, Timestamp: &stamp}}}}
		encoded, _, err := MarshalSourceMetadataV1(record)
		require.NoError(t, err)
		decoded, _, err := DecodeSourceMetadataV1(encoded)
		require.NoError(t, err)
		assert.Equal(t, stamp, *decoded.Fields[0].Value.Timestamp)
	}
}
