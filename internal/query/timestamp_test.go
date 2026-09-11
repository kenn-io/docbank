package query

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Variable-width fractions and zone offsets must not reverse time ordering.
func TestTimestampKeyPreservesNanosecondOrdering(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"2026-01-02T03:04:05Z", "2026-01-02T03:04:05.000000000Z"},
		{"2026-01-02T03:04:05.000000001Z", "2026-01-02T03:04:05.000000001Z"},
		{"2026-01-02T04:04:05.1+01:00", "2026-01-02T03:04:05.100000000Z"},
	} {
		got, err := TimestampKey(test.input)
		require.NoError(t, err)
		require.Equal(t, test.want, got)
	}
	_, err := TimestampKey("not-a-time")
	require.Error(t, err)
}
