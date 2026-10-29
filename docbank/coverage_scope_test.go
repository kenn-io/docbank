package docbank_test

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	docbank "go.kenn.io/docbank/docbank"
	"go.kenn.io/docbank/document/plaintext"
)

func TestEmbeddedCoverageScopeConversion(t *testing.T) {
	t.Parallel()
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{"private": {Profile: embeddedProcessingProfile(t, provider.Descriptor()), RenditionProvider: provider}}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt, err := vault.Put(t.Context(), "/synthetic.txt", strings.NewReader("synthetic coverage"), docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	request := docbank.CoverageRequest{Profile: "private", Fence: docbank.DocumentSourceFence{VaultUID: vault.ID()}, Scope: &docbank.DocumentSearchScope{Kind: "all_live", Filters: &docbank.DocumentSearchFilters{MIMEType: "TEXT/PLAIN"}}}
	scoped, err := vault.DocumentCoverage(t.Context(), request)
	require.NoError(t, err)
	require.NotNil(t, scoped.ObservedScopeCount)
	require.Equal(t, 1, *scoped.ObservedScopeCount)
	require.Equal(t, "text/plain", scoped.Scope.Filters.MIMEType)
	request.Scope = nil
	request.Fence.ContentVersionIDs = []string{receipt.Version.ID}
	exact, err := vault.DocumentCoverage(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, exact.Renditions, scoped.Renditions)
	require.Equal(t, exact.Embeddings, scoped.Embeddings)
	require.Nil(t, exact.Scope)
	require.Nil(t, exact.ObservedScopeCount)
	encoded, err := json.Marshal(exact)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), `"scope"`)
	require.NotContains(t, string(encoded), `"observed_scope_count"`)
	request.Fence.ContentVersionIDs = nil
	request.Scope = &docbank.DocumentSearchScope{Kind: "all_live", Filters: &docbank.DocumentSearchFilters{MIMEType: "application/x-no-match"}}
	empty, err := vault.DocumentCoverage(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "complete", empty.State)
	require.NotNil(t, empty.ObservedScopeCount)
	require.Zero(t, *empty.ObservedScopeCount)
	encoded, err = json.Marshal(empty)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"observed_scope_count":0`)
}
