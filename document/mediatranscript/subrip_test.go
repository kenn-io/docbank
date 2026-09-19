package mediatranscript

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSubRip(t *testing.T) {
	raw := "\ufeff\r\n1\r\n00:00:01,250 --> 00:00:02,750\r\nfirst <i>cue</i>\r\n\r\n2\r\n01:02:03,000 --> 01:02:04,125   \r\nsecond\r\nthird\r\n"
	artifact, err := ParseSubRip([]byte(raw), "loom", "")
	require.NoError(t, err)
	assert.Equal(t, ArtifactV1{
		ContractVersion: contractV1, Origin: "supplied", Provider: "loom",
		Segments: []Segment{
			{Order: 0, StartMS: 1_250, EndMS: 2_750, Text: "first <i>cue</i>"},
			{Order: 1, StartMS: 3_723_000, EndMS: 3_724_125, Text: "second\nthird"},
		},
	}, artifact)

	artifact, err = ParseSubRip([]byte("1\n00:00:00,000 --> 00:00:00,100\ntext\n"), "loom", "en")
	require.NoError(t, err)
	assert.Equal(t, "en", artifact.Language)
}

func TestParseSubRipRejects(t *testing.T) {
	valid := "1\n00:00:00,000 --> 00:00:00,100\ntext"
	tests := map[string]string{
		"empty":            "",
		"whitespace":       " \t\n",
		"invalid utf8":     string([]byte{0xff}),
		"nul":              valid + "\x00",
		"lone cr":          strings.ReplaceAll(valid, "\n", "\r"),
		"zero index":       strings.Replace(valid, "1\n", "0\n", 1),
		"nonnumeric index": strings.Replace(valid, "1\n", "x\n", 1),
		"dot separator":    strings.Replace(valid, ",000", ".000", 1),
		"minute 60":        strings.Replace(valid, "00:00:00,000", "00:60:00,000", 1),
		"hour 24":          strings.Replace(valid, "00:00:00,000", "24:00:00,000", 1),
		"missing arrow":    strings.Replace(valid, " --> ", " -> ", 1),
		"cue settings":     strings.Replace(valid, "00:00:00,100", "00:00:00,100 align:start", 1),
		"end before start": strings.Replace(valid, "00:00:00,100", "00:00:00,000", 1),
		"decreasing start": "1\n00:00:01,000 --> 00:00:02,000\na\n\n2\n00:00:00,000 --> 00:00:01,000\nb",
		"missing text":     "1\n00:00:00,000 --> 00:00:00,100",
		"blank text":       "1\n00:00:00,000 --> 00:00:00,100\n \t",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSubRip([]byte(raw), "loom", "")
			require.Error(t, err)
			if raw != "" {
				assert.NotContains(t, err.Error(), raw)
			}
		})
	}

	tooMany := strings.Builder{}
	for index := 1; index <= maxSegments+1; index++ {
		tooMany.WriteString("1\n00:00:00,000 --> 00:00:00,100\nx\n\n")
	}
	_, err := ParseSubRip([]byte(tooMany.String()), "loom", "")
	require.Error(t, err)
	_, err = ParseSubRip([]byte(strings.Repeat("x", MaxSubRipBytes+1)), "loom", "")
	require.Error(t, err)
}
