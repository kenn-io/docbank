package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseNodeSelector(t *testing.T) {
	path, err := parseNodeSelector("/records/return.pdf")
	require.NoError(t, err)
	assert.Equal(t, "/records/return.pdf", path.path)
	assert.False(t, path.isID())

	id, err := parseNodeSelector("id:42")
	require.NoError(t, err)
	assert.Equal(t, int64(42), id.id)
	assert.True(t, id.isID())
	assert.Equal(t, "id:42", formatNodeSelector(id.id))

	for _, invalid := range []string{"42", "relative/path", "id:", "id:0", "id:-1", "id:01", "id:+1"} {
		_, err := parseNodeSelector(invalid)
		require.Error(t, err, invalid)
		var classified *exitError
		require.ErrorAs(t, err, &classified)
		assert.Equal(t, exitUsage, classified.code)
	}
}
