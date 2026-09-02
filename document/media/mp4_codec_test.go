package media

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
