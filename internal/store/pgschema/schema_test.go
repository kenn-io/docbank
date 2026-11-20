package pgschema

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func TestRejectBrokenMigrationChains(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		files []string
	}{
		{"missing version", []string{"000001_initial.up.sql", "000003_gap.up.sql"}},
		{"repeated version", []string{"000001_initial.up.sql", "000001_other.up.sql"}},
		{"down migration", []string{"000001_initial.down.sql"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := fstest.MapFS{}
			for _, name := range tc.files {
				files["migrations/"+name] = &fstest.MapFile{Data: []byte("SELECT 1;")}
			}
			_, err := loadMigrations(files)
			require.ErrorContains(t, err, "must be forward migration")
		})
	}
}

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
