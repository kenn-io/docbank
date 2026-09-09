package canonical

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sample struct {
	Zulu  string  `json:"zulu"`
	Alpha int64   `json:"alpha"`
	Ratio float64 `json:"ratio,omitzero"`
}

func TestMarshalProducesOneByteForm(t *testing.T) {
	encoded, err := Marshal(sample{Zulu: "z", Alpha: 1 << 60})
	require.NoError(t, err)
	assert.Equal(t, `{"alpha":1152921504606846976,"zulu":"z"}`, string(encoded), //nolint:testifylint // Exact bytes are the contract; JSONEq would accept reordered members.
		"members sort by name and large integers keep their exact digits")

	encoded, err = Marshal(map[string]any{"b": 1.5, "a": []int{2, 1}})
	require.NoError(t, err)
	assert.Equal(t, `{"a":[2,1],"b":1.5}`, string(encoded)) //nolint:testifylint // Exact bytes are the contract.
}

func TestDecodeAcceptsOnlyExactCanonicalBytes(t *testing.T) {
	value, err := Decode[sample]([]byte(`{"alpha":7,"zulu":"z"}`))
	require.NoError(t, err)
	assert.Equal(t, sample{Zulu: "z", Alpha: 7}, value)

	for name, raw := range map[string]string{
		"reordered members": `{"zulu":"z","alpha":7}`,
		"whitespace":        `{"alpha": 7,"zulu":"z"}`,
		"unknown member":    `{"alpha":7,"extra":true,"zulu":"z"}`,
		"duplicate member":  `{"alpha":7,"alpha":8,"zulu":"z"}`,
		"omitted zero kept": `{"alpha":7,"ratio":0,"zulu":"z"}`,
		"trailing bytes":    `{"alpha":7,"zulu":"z"} `,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode[sample]([]byte(raw))
			require.Error(t, err)
		})
	}
}

func TestIsSHA256Hex(t *testing.T) {
	assert.True(t, IsSHA256Hex(strings.Repeat("0f", 32)))
	assert.False(t, IsSHA256Hex(strings.Repeat("0F", 32)), "uppercase is not canonical")
	assert.False(t, IsSHA256Hex(strings.Repeat("0", 63)))
	assert.False(t, IsSHA256Hex(strings.Repeat("g", 64)))
}
