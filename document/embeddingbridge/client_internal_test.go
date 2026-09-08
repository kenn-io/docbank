package embeddingbridge

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyAuthorizedFileDoesNotTransmitOverflowProbe(t *testing.T) {
	var destination bytes.Buffer
	err := copyAuthorizedFile(t.Context(), &destination, strings.NewReader("abcd"), 3, sha256Hex([]byte("abc")))
	require.ErrorIs(t, err, errSourceChanged)
	assert.Equal(t, "abc", destination.String())
}

func TestCopyAuthorizedFileChecksCancellationBetweenReads(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reads := 0
	source := uploadReadFunc(func([]byte) (int, error) {
		reads++
		cancel()
		return 0, nil
	})
	var destination bytes.Buffer
	err := copyAuthorizedFile(ctx, &destination, source, 1, sha256Hex([]byte("a")))
	require.ErrorIs(t, err, errSourceTransferFailed)
	assert.Equal(t, 1, reads)
	assert.Empty(t, destination.Bytes())
}

func TestCopyAuthorizedFileAllowsIntermittentEmptyReads(t *testing.T) {
	content := strings.NewReader("abc")
	reads := 0
	source := uploadReadFunc(func(value []byte) (int, error) {
		reads++
		if reads%100 != 0 {
			return 0, nil
		}
		return content.Read(value[:1])
	})
	var destination bytes.Buffer
	err := copyAuthorizedFile(t.Context(), &destination, source, 3, sha256Hex([]byte("abc")))
	require.NoError(t, err)
	assert.Equal(t, "abc", destination.String())
	assert.Greater(t, reads, 100, "progress resets the consecutive-empty-read limit")
}

type uploadReadFunc func([]byte) (int, error)

func (read uploadReadFunc) Read(value []byte) (int, error) { return read(value) }
