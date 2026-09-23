package loadfile

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestFieldCatalogMatchesOwnedKeysAndResolvesAliases(t *testing.T) {
	entries := FieldCatalog()
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		keys = append(keys, entry.Canonical)
		require.False(t, document.SourceMetadataCanonicalKeyAllowed(entry.Canonical))
	}
	slices.Sort(keys)
	require.Equal(t, fieldCatalogKeys, keys)
	for header, want := range map[string]string{
		"BEGBATES":       "loadfile.label.begin",
		"Begin Doc":      "loadfile.label.begin",
		"Sent Date/Time": "loadfile.date.sent",
		"ModifyDate":     "loadfile.date.modified",
	} {
		entry, ok := ResolveColumn(header)
		require.True(t, ok, header)
		require.Equal(t, want, entry.Canonical)
	}
	for _, key := range []string{"loadfile.date.family", "loadfile.date.sort"} {
		entry, ok := ResolveColumn(key)
		require.True(t, ok)
		require.Empty(t, entry.DateKind)
		require.False(t, entry.Primary)
	}
	role, sensitive, ok := ActorRoleForColumn("BCC")
	require.True(t, ok)
	require.Equal(t, "blind_copy", role)
	require.True(t, sensitive)
	role, sensitive, ok = ActorRoleForColumn("Author")
	require.True(t, ok)
	require.Equal(t, "author", role)
	require.False(t, sensitive)
}

func TestFieldCatalogActorKeyDelegatesToPersonGrammar(t *testing.T) {
	got, err := ActorKey("name_alias", "Ada  Lovelace")
	require.NoError(t, err)
	wantIdentity, err := document.NormalizePersonIdentity("name_alias", "Ada  Lovelace")
	require.NoError(t, err)
	want, err := document.ActorKey(wantIdentity)
	require.NoError(t, err)
	require.Equal(t, want, got)
}
