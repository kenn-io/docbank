package pgschema

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRejectInvalidNamesBeforeConnecting(t *testing.T) {
	t.Parallel()
	for _, c := range []Config{
		{Schema: "", Owner: "owner"},
		{Schema: "public", Owner: "owner"},
		{Schema: "pg_temp", Owner: "owner"},
		{Schema: "information_schema", Owner: "owner"},
		{Schema: "MixedCase", Owner: "owner"},
		{Schema: `docs"; DROP SCHEMA public; --`, Owner: "owner"},
		{Schema: "documents", Owner: ""},
		{Schema: "documents", Owner: `owner"`},
	} {
		_, err := Setup(t.Context(), nil, c)
		require.Error(t, err)
		_, err = Validate(t.Context(), nil, c)
		require.Error(t, err)
	}
}
