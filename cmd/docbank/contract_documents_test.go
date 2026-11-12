package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContractDocumentSelectorRetainsLiteralID(t *testing.T) {
	t.Parallel()
	selector, err := parseNodeSelector("id:42")
	require.NoError(t, err)
	require.EqualValues(t, 42, selector.id)
	require.Empty(t, selector.path)
	for _, raw := range []string{"id:0", "id:01", "relative"} {
		_, err = parseNodeSelector(raw)
		require.Error(t, err)
	}
}
