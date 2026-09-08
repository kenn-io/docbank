package embeddingbridge

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyAuthorizedFileDoesNotTransmitOverflowProbe(t *testing.T) {
	var destination bytes.Buffer
	err := copyAuthorizedFile(&destination, strings.NewReader("abcd"), 3, sha256Hex([]byte("abc")))
	require.ErrorIs(t, err, errSourceChanged)
	assert.Equal(t, "abc", destination.String())
}
