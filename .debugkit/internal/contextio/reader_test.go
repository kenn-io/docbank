package contextio

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReaderStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := strings.NewReader("ab")
	reader := &Reader{Context: ctx, Reader: source}
	buffer := make([]byte, 1)
	n, err := reader.Read(buffer)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	assert.Equal(t, byte('a'), buffer[0])
	cancel()
	n, err = reader.Read(buffer)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, n)
	assert.Equal(t, 1, source.Len())
}
