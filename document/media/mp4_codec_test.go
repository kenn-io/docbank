package media

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestModernCodecConfigurationOwnsItsBytes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind, config string
		data         []byte
	}{
		{"vp09", "vpcC", mediatest.VP9MP4()}, {"av01", "av1C", mediatest.AV1MP4()},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			stsd := bytes.Index(tc.data, []byte("stsd"))
			start := stsd + bytes.Index(tc.data[stsd:], []byte(tc.kind)) - 4
			size := int(binary.BigEndian.Uint32(tc.data[start : start+4]))
			configuration, _, _, ok := visualCodecDimensions(tc.kind, tc.data[start+8:start+size])
			require.True(t, ok)
			expected := slices.Clone(configuration.configuration)
			config := bytes.Index(tc.data, []byte(tc.config)) + 4
			clear(tc.data[config : config+len(expected)])
			assert.Equal(t, expected, configuration.configuration)
		})
	}
}

func TestRemoveEmulationPrevention(t *testing.T) {
	for _, testCase := range []struct {
		name string
		in   []byte
		want []byte
	}{
		{name: "no prevention bytes", in: []byte{0x01, 0x00, 0x02, 0x03}, want: []byte{0x01, 0x00, 0x02, 0x03}},
		{name: "strips one", in: []byte{0x00, 0x00, 0x03, 0x01}, want: []byte{0x00, 0x00, 0x01}},
		{name: "literal three after a stripped byte survives",
			in: []byte{0x00, 0x00, 0x03, 0x03}, want: []byte{0x00, 0x00, 0x03}},
		{name: "stripped byte resets the zero run",
			in: []byte{0x00, 0x00, 0x03, 0x00, 0x03}, want: []byte{0x00, 0x00, 0x00, 0x03}},
		{name: "consecutive sequences",
			in: []byte{0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x02}, want: []byte{0x00, 0x00, 0x00, 0x00, 0x02}},
		{name: "three zeros then three", in: []byte{0x00, 0x00, 0x00, 0x03}, want: []byte{0x00, 0x00, 0x00}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.want, removeEmulationPrevention(testCase.in))
		})
	}
}
