package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAge(t *testing.T) {
	d, err := ParseAge("")
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), d)

	d, err = ParseAge("30d")
	require.NoError(t, err)
	assert.Equal(t, 30*24*time.Hour, d)

	d, err = ParseAge("12h")
	require.NoError(t, err)
	assert.Equal(t, 12*time.Hour, d)

	for _, bad := range []string{"-1d", "-12h", "x", "1w"} {
		_, err = ParseAge(bad)
		assert.Error(t, err, "age %q", bad)
	}
}

// A huge day count overflows int64 nanoseconds; near 2^64 it wraps all the
// way back to a small POSITIVE duration, which the negative check alone
// would accept — and a wrapped-small cutoff makes trash empty delete far
// newer entries than the caller asked to keep.
func TestParseAgeRejectsOverflowingDays(t *testing.T) {
	for _, bad := range []string{"213504d", "106752d", "9223372036854775807d"} {
		_, err := ParseAge(bad)
		require.Error(t, err, "age %q must be rejected, not wrapped", bad)
		assert.Contains(t, err.Error(), "overflow", "age %q", bad)
	}

	// The largest representable day count still parses.
	d, err := ParseAge("106751d")
	require.NoError(t, err)
	assert.Equal(t, 106751*24*time.Hour, d)
}
