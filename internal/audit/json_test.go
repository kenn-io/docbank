package audit

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPortableJSONRoundTripsEveryRegisteredRecord(t *testing.T) {
	for kind := range recordSchemas {
		t.Run(kind, func(t *testing.T) {
			record := exampleRecord(t, kind)
			before, err := Encode(record)
			require.NoError(t, err)
			encoded, err := MarshalJSONRecord(record)
			require.NoError(t, err)
			restored, err := UnmarshalJSONRecord(encoded)
			require.NoError(t, err)
			after, err := Encode(restored)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestPortableJSONUsesCanonicalBase64URLAndNull(t *testing.T) {
	record := Record{Kind: "unknown_origin", Fields: []Field{
		{Name: "node_id", Value: Unsigned(7)},
		{Name: "parent_id", Value: Absent()},
		{Name: "name", Value: Bytes([]byte{0xfb, 0xff})},
	}}
	encoded, err := MarshalJSONRecord(record)
	require.NoError(t, err)
	assert.JSONEq(t, `{"kind":"unknown_origin","fields":{"node_id":7,"parent_id":null,"name":"-_8"}}`, string(encoded))
}

func TestPortableJSONRoundTripsPresentOptionalValues(t *testing.T) {
	record := exampleRecord(t, "content_version")
	record = replaceField(record, "media_type", mustText(t, "application/pdf"))
	record = replaceField(record, "source_version_id", mustUUID(t, "ffeeddcc-bbaa-4988-8766-554433221100"))
	before, err := Encode(record)
	require.NoError(t, err)
	encoded, err := MarshalJSONRecord(record)
	require.NoError(t, err)
	restored, err := UnmarshalJSONRecord(encoded)
	require.NoError(t, err)
	after, err := Encode(restored)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Contains(t, string(encoded), `"media_type":"application/pdf"`)
}

func TestPortableJSONRejectsLossyOrNoncanonicalInput(t *testing.T) {
	valid, err := MarshalJSONRecord(exampleRecord(t, "unknown_origin"))
	require.NoError(t, err)
	tests := map[string]struct {
		raw  string
		want string
	}{
		"unknown record": {
			raw:  `{"kind":"future","fields":{}}`,
			want: "unknown metadata-v1 audit record kind",
		},
		"duplicate nested field": {
			raw:  `{"kind":"unknown_origin","fields":{"node_id":1,"node_id":2,"parent_id":null,"name":null}}`,
			want: "duplicate field",
		},
		"padded base64": {
			raw:  `{"kind":"unknown_origin","fields":{"node_id":1,"parent_id":null,"name":"YQ=="}}`,
			want: "canonical unpadded base64url",
		},
		"exponent integer": {
			raw:  `{"kind":"unknown_origin","fields":{"node_id":1e0,"parent_id":null,"name":null}}`,
			want: "unsigned integer is not canonical",
		},
		"lone surrogate": {
			raw:  `{"kind":"tag_definition","fields":{"tag_id":"00112233-4455-4677-8899-aabbccddeeff","name":"\ud800"}}`,
			want: "unpaired UTF-16 surrogate",
		},
		"required null": {
			raw:  `{"kind":"unknown_origin","fields":{"node_id":null,"parent_id":null,"name":null}}`,
			want: "required unsigned integer value is null",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, parseErr := UnmarshalJSONRecord(json.RawMessage(test.raw))
			require.ErrorContains(t, parseErr, test.want)
		})
	}

	withUnknownField := strings.Replace(string(valid), `"fields":{`, `"extra":true,"fields":{`, 1)
	_, err = UnmarshalJSONRecord(json.RawMessage(withUnknownField))
	require.ErrorContains(t, err, "unknown field")
}
