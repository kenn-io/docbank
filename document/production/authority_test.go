package production_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/pdfstamp"
)

type generatedDaemonTransport interface {
	API() *apiclient.Client
}

var _ generatedDaemonTransport = (*daemonconn.Connection)(nil)

func TestContractsNameCurrentTransportAndPackageAuthorities(t *testing.T) {
	require.Equal(t, "internal/daemonconn.Connection.API", production.GeneratedClientTransport)
	require.Equal(t, "internal/apiclient", production.GeneratedClientPackage)
	require.Equal(t, pdfstamp.RecipeContractV1, production.NumberingStampRecipeContract)

	for _, id := range []string{
		production.LoadfileDATProfileID,
		production.LoadfileOPTProfileID,
		production.LoadfileLFPProfileID,
	} {
		profile, err := loadfile.ReadProfile(id)
		require.NoError(t, err)
		require.Equal(t, id, profile.ID)
		_, err = profile.SHA256()
		require.NoError(t, err)
	}
}
