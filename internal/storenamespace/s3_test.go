package storenamespace

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestS3OverlapsCanonicalEndpointHostAliases(t *testing.T) {
	tests := []struct {
		name  string
		first string
		other string
	}{
		{
			name:  "DNS root dot",
			first: "https://objects.example",
			other: "https://objects.example.",
		},
		{
			name:  "IPv6 spelling",
			first: "https://[2001:db8::1]",
			other: "https://[2001:0db8:0:0:0:0:0:1]",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			overlaps, err := S3Overlaps(
				S3Binding{
					Endpoint: test.first, Bucket: "archive", Prefix: "docbank",
				},
				S3Binding{
					Endpoint: test.other, Bucket: "archive", Prefix: "docbank/nested",
				},
			)
			require.NoError(t, err)
			require.True(t, overlaps)
		})
	}
}
