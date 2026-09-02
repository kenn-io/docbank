package document

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSourceMetadataV1EmptyPayloadsRoundTrip(t *testing.T) {
	record := SourceMetadataV1{ContractVersion: SourceMetadataContractV1, Fields: []SourceMetadataFieldV1{
		{Key: "keywords", Namespace: "pdf.info", SourceField: "Keywords",
			Value: SourceMetadataValueV1{Kind: SourceMetadataStringList, Strings: []string{}}},
		{Key: "title", Namespace: "pdf.info", SourceField: "Title",
			Value: SourceMetadataValueV1{Kind: SourceMetadataString, String: new("")}},
	}, Warnings: []SourceMetadataWarningV1{}}
	encoded, _, err := MarshalSourceMetadataV1(record)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `{"kind":"string","string":""}`)
	assert.Contains(t, string(encoded), `{"kind":"string_list","strings":[]}`)
	decoded, _, err := DecodeSourceMetadataV1(encoded)
	require.NoError(t, err)
	assert.Equal(t, record, decoded)

	for _, missing := range []SourceMetadataValueV1{
		{Kind: SourceMetadataString}, {Kind: SourceMetadataStringList},
	} {
		record.Fields = record.Fields[:1]
		record.Fields[0].Value = missing
		_, _, err := MarshalSourceMetadataV1(record)
		require.ErrorContains(t, err, "conflicting payloads")
	}
}

func TestSourceMetadataV1ZonedTimestampPrecision(t *testing.T) {
	for _, testCase := range []struct {
		stamp SourceMetadataTimestampV1
		want  string
	}{
		{stamp: SourceMetadataTimestampV1{Raw: "2024-01-02T03:04Z", Normalized: "2024-01-02T03:04Z",
			Precision: SourceMetadataPrecisionMinute, Timezone: SourceMetadataTimezoneUTC}},
		{stamp: SourceMetadataTimestampV1{Raw: "2024-01-02T03:04+05:30", Normalized: "2024-01-02T03:04+05:30",
			Offset: "+05:30", Precision: SourceMetadataPrecisionMinute, Timezone: SourceMetadataTimezoneOffset}},
		{stamp: SourceMetadataTimestampV1{Raw: "2024-01-02T03:04:05Z", Normalized: "2024-01-02T03:04:05Z",
			Precision: SourceMetadataPrecisionMinute, Timezone: SourceMetadataTimezoneUTC}, want: "minute precision"},
		{stamp: SourceMetadataTimestampV1{Raw: "2024-01-02T03:04:05.5Z", Normalized: "2024-01-02T03:04:05.5Z",
			Precision: SourceMetadataPrecisionSecond, Timezone: SourceMetadataTimezoneUTC}, want: "fraction"},
		{stamp: SourceMetadataTimestampV1{Raw: "2024-01-02T03:04:05Z", Normalized: "2024-01-02T03:04:05Z",
			Precision: SourceMetadataPrecisionFraction, Timezone: SourceMetadataTimezoneUTC}, want: "fraction"},
		{stamp: SourceMetadataTimestampV1{Raw: "2024-01-02T03:04:05.5", Normalized: "2024-01-02T03:04:05.5",
			Precision: SourceMetadataPrecisionSecond, Timezone: SourceMetadataTimezoneOmitted}, want: "fraction"},
	} {
		t.Run(testCase.stamp.Normalized+"/"+string(testCase.stamp.Precision), func(t *testing.T) {
			record := SourceMetadataV1{ContractVersion: SourceMetadataContractV1, Fields: []SourceMetadataFieldV1{{
				Key: "created", Namespace: "xmp", SourceField: "CreateDate",
				Value: SourceMetadataValueV1{Kind: SourceMetadataTimestamp, Timestamp: &testCase.stamp}}}}
			encoded, _, err := MarshalSourceMetadataV1(record)
			if testCase.want != "" {
				require.ErrorContains(t, err, testCase.want)
				return
			}
			require.NoError(t, err)
			decoded, _, err := DecodeSourceMetadataV1(encoded)
			require.NoError(t, err)
			assert.Equal(t, testCase.stamp, *decoded.Fields[0].Value.Timestamp)
		})
	}
}

// TestSourceMetadataV1RandomRecordsRoundTrip generates canonical records that
// validation accepts and checks that marshal then decode returns them unchanged.
func TestSourceMetadataV1RandomRecordsRoundTrip(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // Seeded generator keeps the property test reproducible.
	for iteration := range 200 {
		record := randomSourceMetadataV1(random)
		encoded, checksum, err := MarshalSourceMetadataV1(record)
		require.NoError(t, err, "iteration %d", iteration)
		decoded, decodedChecksum, err := DecodeSourceMetadataV1(encoded)
		require.NoError(t, err, "iteration %d", iteration)
		assert.Equal(t, checksum, decodedChecksum)
		assert.Equal(t, record, decoded, "iteration %d", iteration)
	}
}

func randomSourceMetadataV1(random *rand.Rand) SourceMetadataV1 {
	keys := []string{"title", "subject", "keywords", "creators", "page_count", "created",
		"modified", "image.exif.iso", "media.container.duration", "office.custom.rating"}
	random.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	keys = keys[:random.IntN(len(keys)+1)]
	slices.Sort(keys)
	record := SourceMetadataV1{ContractVersion: SourceMetadataContractV1,
		Fields: []SourceMetadataFieldV1{}, Warnings: []SourceMetadataWarningV1{}}
	for _, key := range keys {
		record.Fields = append(record.Fields, SourceMetadataFieldV1{
			Key: key, Namespace: "office.custom", SourceField: randomLabel(random, 12),
			Sensitive: random.IntN(2) == 1, Value: randomSourceMetadataValue(random)})
	}
	if random.IntN(2) == 1 {
		record.Warnings = append(record.Warnings, SourceMetadataWarningV1{
			Code: randomLabel(random, 8), Namespace: "office.custom",
			SourceField: randomLabel(random, 8), Detail: randomASCII(random, 0, 40)})
	}
	return record
}

func randomSourceMetadataValue(random *rand.Rand) SourceMetadataValueV1 {
	switch random.IntN(6) {
	case 0:
		return SourceMetadataValueV1{Kind: SourceMetadataString, String: new(randomASCII(random, 0, 32))}
	case 1:
		values := make([]string, random.IntN(4))
		for index := range values {
			values[index] = randomASCII(random, 0, 16)
		}
		return SourceMetadataValueV1{Kind: SourceMetadataStringList, Strings: values}
	case 2:
		return SourceMetadataValueV1{Kind: SourceMetadataInteger, Integer: new(random.Int64() - math.MaxInt64/2)}
	case 3:
		return SourceMetadataValueV1{Kind: SourceMetadataNumber, Number: new(random.NormFloat64() * 1e6)}
	case 4:
		return SourceMetadataValueV1{Kind: SourceMetadataBoolean, Boolean: new(random.IntN(2) == 1)}
	default:
		return SourceMetadataValueV1{Kind: SourceMetadataTimestamp, Timestamp: randomSourceMetadataTimestamp(random)}
	}
}

func randomSourceMetadataTimestamp(random *rand.Rand) *SourceMetadataTimestampV1 {
	instant := time.Date(1990+random.IntN(60), time.Month(1+random.IntN(12)), 1+random.IntN(28),
		random.IntN(24), random.IntN(60), random.IntN(60), 1_000_000*(1+random.IntN(999)), time.UTC)
	precisions := []SourceMetadataTimestampPrecision{SourceMetadataPrecisionDate, SourceMetadataPrecisionHour,
		SourceMetadataPrecisionMinute, SourceMetadataPrecisionSecond, SourceMetadataPrecisionFraction}
	precision := precisions[random.IntN(len(precisions))]
	stamp := &SourceMetadataTimestampV1{Precision: precision, Timezone: SourceMetadataTimezoneOmitted}
	layout := sourceMetadataPrecisionLayouts[precision]
	zonable := precision != SourceMetadataPrecisionDate && precision != SourceMetadataPrecisionHour
	switch {
	case !zonable || random.IntN(3) == 0:
	case random.IntN(2) == 0:
		stamp.Timezone = SourceMetadataTimezoneUTC
		layout += "Z"
	default:
		stamp.Timezone = SourceMetadataTimezoneOffset
		hours, minutes := random.IntN(29)-14, []int{0, 30, 45}[random.IntN(3)]
		if hours == 14 || hours == -14 {
			minutes = 0
		}
		stamp.Offset = fmt.Sprintf("%+03d:%02d", hours, minutes)
		layout += "-07:00"
		offsetSeconds := hours*3600 + minutes*60
		if hours < 0 {
			offsetSeconds = hours*3600 - minutes*60
		}
		instant = instant.In(time.FixedZone("", offsetSeconds))
	}
	stamp.Normalized = instant.Format(layout)
	stamp.Raw = stamp.Normalized
	return stamp
}

func randomLabel(random *rand.Rand, maximum int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJ0123456789-_."
	out := make([]byte, 1+random.IntN(maximum))
	for index := range out {
		out[index] = alphabet[random.IntN(len(alphabet))]
	}
	return string(out)
}

func randomASCII(random *rand.Rand, minimum, maximum int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz ABCDEFGHIJ0123456789-_./"
	length := minimum + random.IntN(maximum-minimum+1)
	out := make([]byte, length)
	for index := range out {
		out[index] = alphabet[random.IntN(len(alphabet))]
	}
	return string(out)
}

func FuzzDecodeSourceMetadataV1(f *testing.F) {
	golden, err := os.ReadFile(filepath.Join("testdata", "source-metadata-v1.golden.json"))
	require.NoError(f, err)
	f.Add(golden)
	random := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // Seeded generator keeps the fuzz corpus reproducible.
	for range 8 {
		encoded, _, err := MarshalSourceMetadataV1(randomSourceMetadataV1(random))
		require.NoError(f, err)
		f.Add(encoded)
	}
	f.Add([]byte(`{"contract_version":"source-metadata/v1","fields":[],"warnings":[]}`))
	f.Add([]byte(`{"contract_version":"source-metadata/v1","fields":[{"key":"title","namespace":"pdf.info",` +
		`"sensitive":false,"source_field":"T","value":{"kind":"string","string":"x","strings":[]}}],"warnings":[]}`))
	f.Fuzz(func(t *testing.T, encoded []byte) {
		decoded, checksum, err := DecodeSourceMetadataV1(encoded)
		if err != nil {
			return
		}
		encodedAgain, checksumAgain, err := MarshalSourceMetadataV1(decoded)
		require.NoError(t, err)
		assert.Equal(t, encoded, encodedAgain)
		assert.Equal(t, checksum, checksumAgain)
	})
}
