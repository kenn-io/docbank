package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatHumanTimestampUsesUTCSecondPrecision(t *testing.T) {
	got, err := formatHumanTimestamp("2026-07-07T13:07:31.870853000-05:00")
	require.NoError(t, err)
	assert.Equal(t, "2026-07-07T18:07:31Z", got)

	_, err = formatHumanTimestamp("not-a-timestamp")
	require.ErrorContains(t, err, "formatting timestamp")
}
