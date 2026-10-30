package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestPrintChanges(t *testing.T) {
	t.Parallel()
	watch := func(receive func(store.VaultChange) error) error {
		return receive(store.VaultChange{Cursor: "saved", Kind: "created", NodeID: 42, Revision: 3})
	}
	var out bytes.Buffer
	require.NoError(t, printChanges(&out, false, watch))
	require.Equal(t, "saved\tcreated\t42\t3\n", out.String())
	out.Reset()
	require.NoError(t, printChanges(&out, true, watch))
	require.JSONEq(t, "{\"cursor\":\"saved\",\"kind\":\"created\",\"node_id\":42,\"revision\":3}\n", out.String())
}
