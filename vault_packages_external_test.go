package docbank_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank"
)

func TestEmbeddedPackageImportAndReadAPI(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001", "DATA"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001", "NATIVES"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "DATA", "load.dat"),
		[]byte("þDOCIDþ\x14þNATIVEþ\x14þBEGBATESþ\r\nþDOC-Aþ\x14þNATIVES/DOC-A.txtþ\x14þEXT000001þ\r\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "NATIVES", "DOC-A.txt"), []byte("synthetic document\n"), 0o600))
	mapping := []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"DOCID","source_ordinal":0,"canonical":"loadfile.document.id"},{"source":"NATIVE","source_ordinal":1,"canonical":"loadfile.file.native"},{"source":"BEGBATES","source_ordinal":2,"canonical":"loadfile.label.begin"}]}`)
	preflight, err := vault.PreflightPackage(t.Context(), docbank.PackagePreflightRequest{
		SourceKind: "root", SourceRef: root, Profile: "dat-concordance-v1", Encoding: "utf-8", Mapping: mapping,
	})
	require.NoError(t, err)
	assert.False(t, preflight.Blocking)
	retained, err := vault.PackagePreflight(t.Context(), preflight.PreflightID)
	require.NoError(t, err)
	assert.Equal(t, preflight.PreflightID, retained.PreflightID)

	operationID := uuid.NewString()
	_, err = vault.ImportPackage(t.Context(), docbank.PackageImportRequest{
		PreflightID: preflight.PreflightID, Into: "/", Name: "synthetic-package", OperationID: operationID,
	})
	require.NoError(t, err)
	var status docbank.PackageImportJob
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		status, err = vault.PackageImportStatus(t.Context(), operationID)
		require.NoError(t, err)
		if status.State == "complete" || status.State == "partial" || status.State == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Equal(t, "complete", status.State)

	packages, err := vault.Packages(t.Context(), docbank.PackageListRequest{Direction: "received", Limit: 100})
	require.NoError(t, err)
	require.Len(t, packages.Items, 1)
	pkg, err := vault.Package(t.Context(), packages.Items[0].PackageID)
	require.NoError(t, err)
	assert.Equal(t, status.PackageID, pkg.PackageID)
	_, err = vault.PackageMembers(t.Context(), docbank.PackageMemberRequest{PackageID: pkg.PackageID, Limit: 500})
	require.ErrorIs(t, err, docbank.ErrInvalidArgument)
	members, err := vault.PackageMembers(t.Context(), docbank.PackageMemberRequest{PackageID: pkg.PackageID, Limit: 100})
	require.NoError(t, err)
	require.Len(t, members.Items, 1)
	require.NotEmpty(t, members.Items[0].RowID)
	record, err := vault.PackageRecord(t.Context(), pkg.PackageID, members.Items[0].RowID)
	require.NoError(t, err)
	assert.Equal(t, "EXT000001", record.Columns["BEGBATES"])
	require.Len(t, record.Fields, 3)
	assert.Equal(t, "BEGBATES", record.Fields[2].Column)

	lookup, err := vault.LookupLabel(t.Context(), docbank.LabelLookupRequest{
		Label: "EXT000001", PackageID: pkg.PackageID, Provenance: "received", Limit: 100,
	})
	require.NoError(t, err)
	require.Len(t, lookup.Matches, 1)
	assert.Equal(t, members.Items[0].OccurrenceID, lookup.Matches[0].OccurrenceID)
}
