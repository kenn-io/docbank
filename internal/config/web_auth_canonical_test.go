package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalWebOriginNormalizesBrowserAuthority(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "expanded IPv6 and padded port",
			input: "http://[2001:0db8:0000:0000:0000:0000:0000:0001]:08080",
			want:  "http://[2001:db8::1]:8080",
		},
		{
			name:  "IPv4-mapped IPv6 browser serialization",
			input: "http://[::ffff:127.0.0.1]:7777",
			want:  "http://[::ffff:7f00:1]:7777",
		},
		{
			name:  "expanded IPv6 and padded default HTTPS port",
			input: "https://[2001:0db8:0:0:0:0:0:1]:00443",
			want:  "https://[2001:db8::1]",
		},
		{
			name:  "padded default HTTP port",
			input: "http://Archive.example.test:00080",
			want:  "http://archive.example.test",
		},
		{
			name:  "padded nondefault port",
			input: "http://Archive.example.test:08080",
			want:  "http://archive.example.test:8080",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := CanonicalWebOrigin(test.input)
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

func TestWebHostAuthorityNormalizesIPAddressAndNumericPort(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"[2001:0DB8:0000:0000:0000:0000:0000:0001]:08080", "[2001:db8::1]:8080"},
		{"[::ffff:127.0.0.1]:07777", "[::ffff:7f00:1]:7777"},
		{"Archive.example.test:0080", "archive.example.test:80"},
		{"LOCALHOST", "localhost"},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := WebHostAuthority(test.input)
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}
