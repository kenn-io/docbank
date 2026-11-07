package docbank

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSearchDocumentsValidity(t *testing.T) {
	t.Parallel()
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	old, err := vault.Put(t.Context(), "/renewal.txt", strings.NewReader("synthetic original"), PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	replacement, err := vault.Put(t.Context(), "/replacement.txt", strings.NewReader("synthetic replacement"), PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	_, err = vault.metadata.PutEffectiveWindow(t.Context(), replacement.Node.ID, replacement.Node.Revision, "2025-01-01", "")
	require.NoError(t, err)
	window, err := vault.metadata.PutEffectiveWindow(t.Context(), old.Node.ID, old.Node.Revision, "2024-01-01", "2026-01-01")
	require.NoError(t, err)
	_, err = vault.metadata.PutSupersession(t.Context(), replacement.Node.ID, old.Node.ID, window.Node.Revision, new(""))
	require.NoError(t, err)
	results, err := vault.SearchDocuments(t.Context(), DocumentSearchRequest{
		Query: "renewal", Mode: DocumentSearchLexical, Profile: "supplied-captions", Limit: 10, AsOf: "2025-06-01",
		Fence: DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{old.Version.ID, replacement.Version.ID}},
	})
	require.NoError(t, err)
	require.Len(t, results.Results, 1)
	require.Equal(t, "2025-06-01", results.AsOf)
	require.Equal(t, &DocumentResultValidity{State: "superseded", EffectiveFrom: "2024-01-01", EffectiveTo: "2026-01-01",
		SupersededBy: []DocumentValiditySuccessor{{NodeID: replacement.Node.ID, Path: "/replacement.txt", EffectiveFrom: "2025-01-01"}}}, results.Results[0].Validity)
}
