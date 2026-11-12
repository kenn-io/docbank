package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
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

func TestContractShowIncludesDocumentEvidence(t *testing.T) {
	var out bytes.Buffer
	cmd := newContractShowCommand()
	cmd.SetOut(&out)
	c := api.Contract{Title: "Agreement", Documents: []api.ContractDocument{{NodeID: 42, Role: "signed", ContentVersionID: "version-id", BlobSHA256: "hash", Current: false, Available: true}}}
	require.NoError(t, printContractShow(cmd, c, false))
	for _, want := range []string{"signed", "id:42", "version-id", "hash", "current=false", "available=true"} {
		require.Contains(t, out.String(), want)
	}
}
