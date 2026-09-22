package api

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPackageImportDigestIncludesConsentAndDestination(t *testing.T) {
	first := PackageImportRequest{PreflightID: "preview", Into: "/received", Name: "synthetic"}
	a, err := packageImportRequestHash(first)
	require.NoError(t, err)
	first.IndexSuppliedText = true
	b, err := packageImportRequestHash(first)
	require.NoError(t, err)
	require.NotEqual(t, a, b)
	first.Into = "/different"
	c, err := packageImportRequestHash(first)
	require.NoError(t, err)
	require.NotEqual(t, b, c)
	first.AcceptPartial = true
	d, err := packageImportRequestHash(first)
	require.NoError(t, err)
	require.NotEqual(t, c, d)
}
