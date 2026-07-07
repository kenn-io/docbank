package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"plain", "report.pdf", "report.pdf", false},
		{"unicode nfc kept", "café.txt", "café.txt", false},
		// NFD "café" (e + combining acute) normalizes to NFC.
		{"nfd normalized", "café.txt", "café.txt", false},
		{"case preserved", "README.md", "README.md", false},
		{"empty", "", "", true},
		{"dot", ".", "", true},
		{"dotdot", "..", "", true},
		{"slash", "a/b", "", true},
		{"nul", "a\x00b", "", true},
		{"dotfile ok", ".bashrc", ".bashrc", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeName(tt.in)
			if tt.wantErr {
				require.ErrorIs(t, err, ErrInvalidName)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSuffixRoundTrip(t *testing.T) {
	base, ext := splitSuffix("report.pdf")
	assert.Equal(t, "report", base)
	assert.Equal(t, ".pdf", ext)

	base, ext = splitSuffix(".bashrc")
	assert.Equal(t, ".bashrc", base)
	assert.Empty(t, ext)

	base, ext = splitSuffix("archive.tar.gz")
	assert.Equal(t, "archive.tar", base)
	assert.Equal(t, ".gz", ext)

	assert.Equal(t, "report.pdf", suffixedName("report", ".pdf", 1))
	assert.Equal(t, "report (2).pdf", suffixedName("report", ".pdf", 2))

	n, ok := parseSuffix("report.pdf", "report", ".pdf")
	assert.True(t, ok)
	assert.Equal(t, 1, n)
	n, ok = parseSuffix("report (7).pdf", "report", ".pdf")
	assert.True(t, ok)
	assert.Equal(t, 7, n)
	_, ok = parseSuffix("other.pdf", "report", ".pdf")
	assert.False(t, ok)
	_, ok = parseSuffix("report (x).pdf", "report", ".pdf")
	assert.False(t, ok)
}
