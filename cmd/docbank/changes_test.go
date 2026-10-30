package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestPrintChanges(t *testing.T) {
	t.Parallel()
	watch := func(receive func(store.VaultChange) error) (string, error) {
		return "checkpoint", receive(store.VaultChange{Cursor: "saved", Kind: "created", NodeID: 42, Revision: 3})
	}
	var out, stderr bytes.Buffer
	require.NoError(t, printChanges(&out, &stderr, false, watch))
	require.Equal(t, "saved\tcreated\t42\t3\n", out.String())
	require.Equal(t, "cursor: checkpoint\n", stderr.String())
	out.Reset()
	stderr.Reset()
	require.NoError(t, printChanges(&out, &stderr, true, watch))
	require.JSONEq(t, "{\"cursor\":\"saved\",\"kind\":\"created\",\"node_id\":42,\"revision\":3}\n", out.String())
}
