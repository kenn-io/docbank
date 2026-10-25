package pgschema

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRejectInvalidNamesBeforeConnecting(t *testing.T) {
	t.Parallel()
	for _, c := range []Config{
		{Schema: "", SchemaOwner: "owner"},
		{Schema: "public", SchemaOwner: "owner"},
		{Schema: "pg_temp", SchemaOwner: "owner"},
		{Schema: "information_schema", SchemaOwner: "owner"},
		{Schema: "MixedCase", SchemaOwner: "owner"},
		{Schema: `docs"; DROP SCHEMA public; --`, SchemaOwner: "owner"},
		{Schema: "documents", SchemaOwner: ""},
		{Schema: "documents", SchemaOwner: "owner\x00"},
	} {
		_, err := Setup(t.Context(), nil, c)
		require.Error(t, err)
		_, err = Validate(t.Context(), nil, c)
		require.Error(t, err)
	}
}
