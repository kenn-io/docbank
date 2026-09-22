package docbank_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank"
	"go.kenn.io/docbank/internal/packagetest"
)

func TestEmbeddedPackageExportRoundTripsThroughFreshVault(t *testing.T) {
	source, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })

	production := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(production, "VOL001", "DATA"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(production, "VOL001", "NATIVES"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(production, "VOL001", "DATA", "load.dat"), []byte(
		"þDOCIDþ\x14þPARENTIDþ\x14þNATIVEþ\r\n"+
			"þDOC-Aþ\x14þþ\x14þNATIVES/DOC-A.txtþ\r\n"+
			"þDOC-Bþ\x14þDOC-Aþ\x14þNATIVES/DOC-B.txtþ\r\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(production, "VOL001", "NATIVES", "DOC-A.txt"), []byte("synthetic parent\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(production, "VOL001", "NATIVES", "DOC-B.txt"), []byte("synthetic child\n"), 0o600))
	mapping := []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"DOCID","source_ordinal":0,"canonical":"loadfile.document.id"},{"source":"PARENTID","source_ordinal":1,"canonical":"loadfile.family.parent"},{"source":"NATIVE","source_ordinal":2,"canonical":"loadfile.file.native"}]}`)
	preflight, err := source.PreflightPackage(t.Context(), docbank.PackagePreflightRequest{
		SourceKind: "root", SourceRef: production, Profile: "dat-concordance-v1", Encoding: "utf-8", Mapping: mapping,
	})
	require.NoError(t, err)
	require.False(t, preflight.Blocking)
	status := importEmbeddedPackage(t, source, preflight.PreflightID, "round-trip-source")
	require.Equal(t, "complete", status.State)
	sourcePackages, err := source.Packages(t.Context(), docbank.PackageListRequest{Direction: "received", Limit: 10})
	require.NoError(t, err)
	require.Len(t, sourcePackages.Items, 1)
	sourceMembers, err := source.PackageMembers(t.Context(), docbank.PackageMemberRequest{
		PackageID: sourcePackages.Items[0].PackageID, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, sourceMembers.Items, 2)

	var archive bytes.Buffer
	receipt, err := source.ExportPackage(t.Context(), docbank.PackageExportRequest{
		SnapshotID: sourcePackages.Items[0].SnapshotID, SourcePackageID: sourcePackages.Items[0].PackageID,
		ProfileID: "export-csv-natives-v1",
	}, &archive)
	require.NoError(t, err)
	assert.Equal(t, 2, receipt.RecordCount)
	assert.NotEmpty(t, receipt.ArchiveSHA256)
	assert.Equal(t, int64(archive.Len()), receipt.Size)

	independent, err := packagetest.ReadLoadFilePackage(archive.Bytes())
	require.NoError(t, err)
	require.Equal(t, 2, independent.Records)
	require.Len(t, independent.Rows, 3)
	assert.Equal(t, "DOCID", independent.Rows[0][0])
	assert.Equal(t, "DOC000001", independent.Rows[1][0])
	assert.Equal(t, "DOC000001", independent.Rows[2][1])

	extracted := t.TempDir()
	require.NoError(t, independent.Extract(extracted))
	fresh, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	freshPreflight, err := fresh.PreflightPackage(t.Context(), docbank.PackagePreflightRequest{
		SourceKind: "root", SourceRef: extracted, Profile: "csv-rfc4180-v1", Encoding: "utf-8",
		Mapping: independent.MappingJSON,
	})
	require.NoError(t, err)
	require.False(t, freshPreflight.Blocking)
	freshStatus := importEmbeddedPackage(t, fresh, freshPreflight.PreflightID, "round-trip-fresh")
	require.Equal(t, "complete", freshStatus.State)
	freshPackages, err := fresh.Packages(t.Context(), docbank.PackageListRequest{Direction: "received", Limit: 10})
	require.NoError(t, err)
	require.Len(t, freshPackages.Items, 1)
	freshMembers, err := fresh.PackageMembers(t.Context(), docbank.PackageMemberRequest{
		PackageID: freshPackages.Items[0].PackageID, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, freshMembers.Items, 2)
	assert.Equal(t, sourceMembers.Items[0].Representations[0].BlobSHA256, freshMembers.Items[0].Representations[0].BlobSHA256)
	assert.Equal(t, sourceMembers.Items[1].Representations[0].BlobSHA256, freshMembers.Items[1].Representations[0].BlobSHA256)
	assert.Equal(t, freshMembers.Items[0].OccurrenceID, freshMembers.Items[1].ParentOccurrenceID)
	assert.Equal(t, freshMembers.Items[0].FamilyID, freshMembers.Items[1].FamilyID)

	namespaces, err := fresh.BatesNamespaces(t.Context(), docbank.BatesNamespaceListRequest{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, namespaces.Items, "importing a produced package must not allocate new Bates labels")
}

func importEmbeddedPackage(t *testing.T, vault *docbank.Vault, preflightID, name string) docbank.PackageImportJob {
	t.Helper()
	operationID := uuid.NewString()
	_, err := vault.ImportPackage(t.Context(), docbank.PackageImportRequest{
		PreflightID: preflightID, Into: "/", Name: name, OperationID: operationID,
	})
	require.NoError(t, err)
	var status docbank.PackageImportJob
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		status, err = vault.PackageImportStatus(t.Context(), operationID)
		require.NoError(t, err)
		if status.State == "complete" || status.State == "partial" || status.State == "failed" {
			return status
		}
		time.Sleep(20 * time.Millisecond)
	}
	return status
}
