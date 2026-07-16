package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditEncodingBoundaryGoldenHashes(t *testing.T) {
	vectors := map[string]Value{
		"absent":             Absent(),
		"empty_bytes":        Bytes(nil),
		"empty_list":         List(),
		"empty_text":         mustText(t, ""),
		"signed_max":         Signed(math.MaxInt64),
		"signed_min":         Signed(math.MinInt64),
		"unicode_composed":   mustText(t, "é"),
		"unicode_decomposed": mustText(t, "e\u0301"),
		"unsigned_max":       Unsigned(math.MaxUint64),
	}
	expected := map[string]string{
		"absent":             "e246da6d063646289c5a08180d0f685a8ff152c680553d86ad280e4e212f062e",
		"empty_bytes":        "aaedf143074c3a65ad6a94f16e974223390ef9d6cb3fc8449a9c07c91037ee24",
		"empty_list":         "c95ea7ae732f937c28ded8c48c09ea9f8c2eb784e99de93b9184959134cba5dc",
		"empty_text":         "330c2d7df55d0d3583a1cf6b26dae71e3429fe82c10f566663f04e5ff30ac1b7",
		"signed_max":         "91e9ad2ef56645236d1ee3fdb1b5cdec743f0f3cef0af02a31b139d568c812f9",
		"signed_min":         "c3d84acdadc490c2c5e32409ff04cbfd7f3eb512a1a1a4a6d0200e7dd6ca7ab0",
		"unicode_composed":   "185b8fc359601a7de2a9792f57ac01c4b8cf3cd956f6a02188e865d30ddfa7ee",
		"unicode_decomposed": "fe5263c08721131adbe799c69df428d224152619ae5b7379303ebeda8c2e1cd0",
		"unsigned_max":       "4bd65a28a85970bd928f51b80b23f72ddb4ad15c475d8f2299ca89f75b964e3a",
	}
	for name, value := range vectors {
		t.Run(name, func(t *testing.T) {
			digest, err := Hash(Record{Kind: "vector", Fields: []Field{{Name: "value", Value: value}}})
			require.NoError(t, err)
			actual := hex.EncodeToString(digest[:])
			if actual != expected[name] {
				t.Errorf("%s = %s", name, actual)
			}
		})
	}
}

func TestAuditEncodingGoldenRecord(t *testing.T) {
	timestamp := mustTimestamp(t, "2026-07-16T12:34:56.123456789Z")
	uuid := mustUUID(t, "00112233-4455-4677-8899-aabbccddeeff")
	digest := sha256.Sum256([]byte("docbank"))
	text := mustText(t, "hé")
	listText := mustText(t, "x")
	record := Record{Kind: "golden_record", Fields: []Field{
		{Name: "z_absent", Value: Absent()},
		{Name: "k_nested", Value: Nested(Record{Kind: "child", Fields: []Field{
			{Name: "value", Value: Bool(true)},
		}})},
		{Name: "j_list", Value: List(Absent(), Unsigned(9), listText)},
		{Name: "i_digest", Value: Digest(digest)},
		{Name: "h_uuid", Value: uuid},
		{Name: "g_timestamp", Value: timestamp},
		{Name: "f_text", Value: text},
		{Name: "e_bytes", Value: Bytes([]byte{0x00, 0xff})},
		{Name: "d_signed", Value: Signed(-2)},
		{Name: "c_unsigned", Value: Unsigned(0x0102030405060708)},
		{Name: "b_true", Value: Bool(true)},
		{Name: "a_false", Value: Bool(false)},
	}}
	encoded, err := Encode(record)
	require.NoError(t, err)
	actual := hex.EncodeToString(encoded)
	const expected = "000000000000000d646f6362616e6b2d61756469740000000000000001000000000000000d676f6c64656e5f7265636f7264000000000000000c0000000000000007615f66616c7365010000000000000006625f7472756502000000000000000a635f756e7369676e65640301020304050607080000000000000008645f7369676e656404fffffffffffffffe0000000000000007655f627974657305000000000000000200ff0000000000000006665f7465787406000000000000000368c3a9000000000000000b675f74696d657374616d7007000000000000001e323032362d30372d31365431323a33343a35362e3132333435363738395a0000000000000006685f757569640800112233445546778899aabbccddeeff0000000000000008695f64696765737409d1e815eb101fa389ebba332f74729f1aca88a97e8f814d007c63541fd241e5b800000000000000066a5f6c6973740a0000000000000003000300000000000000090600000000000000017800000000000000086b5f6e65737465640b0000000000000040000000000000000d646f6362616e6b2d6175646974000000000000000100000000000000056368696c640000000000000001000000000000000576616c75650200000000000000087a5f616273656e7400"
	if actual != expected {
		t.Fatalf("audit encoding golden bytes = %s", actual)
	}
	digestValue, err := Hash(record)
	require.NoError(t, err)
	assert.Equal(t, "5c851f9926b68c6689637d82cbe8d541c2703aa6ded1f04e5bb1650cbbfbc4cc",
		hex.EncodeToString(digestValue[:]))
}

func TestAuditEncodingSortsFieldsAndDistinguishesAbsentFromEmpty(t *testing.T) {
	emptyText := mustText(t, "")
	left, err := Encode(Record{Kind: "sample", Fields: []Field{
		{Name: "z", Value: Bytes(nil)},
		{Name: "a", Value: Absent()},
		{Name: "m", Value: emptyText},
	}})
	require.NoError(t, err)
	right, err := Encode(Record{Kind: "sample", Fields: []Field{
		{Name: "m", Value: emptyText},
		{Name: "z", Value: Bytes([]byte{})},
		{Name: "a", Value: Absent()},
	}})
	require.NoError(t, err)
	assert.Equal(t, left, right)

	absent, err := Encode(Record{Kind: "sample", Fields: []Field{{Name: "a", Value: Absent()}}})
	require.NoError(t, err)
	empty, err := Encode(Record{Kind: "sample", Fields: []Field{{Name: "a", Value: Bytes(nil)}}})
	require.NoError(t, err)
	assert.NotEqual(t, absent, empty)
}

func TestAuditEncodingValuesOwnInputBytes(t *testing.T) {
	input := []byte("before")
	value := Bytes(input)
	input[0] = 'x'
	encoded, err := Encode(Record{Kind: "sample", Fields: []Field{{Name: "value", Value: value}}})
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "before")
	assert.NotContains(t, string(encoded), "xefore")

	nested := Record{Kind: "child", Fields: []Field{{Name: "value", Value: Bytes([]byte("before"))}}}
	nestedValue := Nested(nested)
	nested.Fields[0].Value = Bytes([]byte("after"))
	encoded, err = Encode(Record{Kind: "sample", Fields: []Field{{Name: "child", Value: nestedValue}}})
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "before")
	assert.NotContains(t, string(encoded), "after")
}

func TestAuditEncodingRejectsInvalidScalars(t *testing.T) {
	_, err := Text(string([]byte{0xff}))
	require.ErrorContains(t, err, "UTF-8")
	for _, value := range []string{
		"2026-07-16T12:34:56Z",
		"2026-07-16T12:34:56.123456789+00:00",
		"2026-07-16T12:34:56.12345678Z",
	} {
		_, err = Timestamp(value)
		require.ErrorContains(t, err, "canonical UTC", value)
	}
	for _, value := range []string{
		"00112233-4455-4677-8899-AABBCCDDEEFF",
		"00112233-4455-3677-8899-aabbccddeeff",
		"00112233-4455-4677-0899-aabbccddeeff",
		"00112233445546778899aabbccddeeff",
	} {
		_, err = UUID(value)
		require.Error(t, err, value)
	}
	for _, value := range []string{strings.Repeat("a", 63), strings.Repeat("A", 64)} {
		_, err = DigestHex(value)
		require.ErrorContains(t, err, "canonical lowercase", value)
	}
}

func TestAuditEncodingRejectsInvalidRecordShape(t *testing.T) {
	for name, record := range map[string]Record{
		"empty kind": {Fields: []Field{{Name: "value", Value: Absent()}}},
		"kind case":  {Kind: "Bad", Fields: []Field{{Name: "value", Value: Absent()}}},
		"field case": {Kind: "good", Fields: []Field{{Name: "Bad", Value: Absent()}}},
		"duplicate": {Kind: "good", Fields: []Field{
			{Name: "value", Value: Absent()}, {Name: "value", Value: Bool(false)},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Encode(record)
			require.Error(t, err)
		})
	}
}

func TestAuditEncodingBoundsRecursiveValues(t *testing.T) {
	value := Absent()
	for range maxValueDepth + 2 {
		value = List(value)
	}
	_, err := Encode(Record{Kind: "deep", Fields: []Field{{Name: "value", Value: value}}})
	require.ErrorContains(t, err, "nesting exceeds")
}

func mustText(t *testing.T, value string) Value {
	t.Helper()
	encoded, err := Text(value)
	require.NoError(t, err)
	return encoded
}

func mustTimestamp(t *testing.T, value string) Value {
	t.Helper()
	encoded, err := Timestamp(value)
	require.NoError(t, err)
	return encoded
}

func mustUUID(t *testing.T, value string) Value {
	t.Helper()
	encoded, err := UUID(value)
	require.NoError(t, err)
	return encoded
}
