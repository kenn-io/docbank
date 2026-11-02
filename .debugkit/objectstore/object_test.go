package objectstore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"
)

func TestValidateKey(t *testing.T) {
	for _, key := range []string{"", ".", "..", "../escape", "a/../escape", "/absolute", "a//b", "a/", `a\b`, `C:escape`, "a\x00b"} {
		require.Error(t, objectstore.ValidateKey(key), "key %q", key)
	}
	for _, key := range []string{"a", "a/b", "foreign.name", "a-b_c", "CON", "nul.txt"} {
		require.NoError(t, objectstore.ValidateKey(key), "key %q", key)
	}
}
