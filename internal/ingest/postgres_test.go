package ingest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/pgstoretest"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func postgresIngesterFixture(t *testing.T, options blob.Options) *Ingester {
	t.Helper()
	setup, owner := pgtest.NewDatabase(t)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	connection := pgstoretest.RuntimeConfig(t, setup, cfg)
	dir := t.TempDir()
	s, err := store.OpenPostgres(t.Context(), connection, cfg, dir)
	require.NoError(t, err)
	return ingesterFixtureWithStore(t, s, dir, options)
}

func TestFilesystemImportPostgres(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name  string
		check func(*testing.T, ingesterFixtureFactory)
	}{
		{"AddSingleFile", checkAddSingleFile},
		{"AddDirectoryPreservesStructure", checkAddDirectoryPreservesStructure},
		{"AddRerunConverges", checkAddRerunConverges},
		{"AddReplaceVersionsDestinationWithoutSuffix", checkAddReplaceVersionsDestinationWithoutSuffix},
		{"AddWithoutReplaceStillSuffixesChangedContent", checkAddWithoutReplaceStillSuffixesChangedContent},
		{"AddReplaceSkipsUnchangedBytesAndStoredMIME", checkAddReplaceSkipsUnchangedBytesAndStoredMIME},
		{"AddDirectoryTrailingSlash", checkAddDirectoryTrailingSlash},
		{"AddPathsHonorsPreflightExclusions", checkAddPathsHonorsPreflightExclusions},
		{"AddContinuesPastDirCollision", checkAddContinuesPastDirCollision},
		{"AddMissingSourceIsReportedNotFatal", checkAddMissingSourceIsReportedNotFatal},
		{"AddTreeStaleDestinationIsNotResurrected", checkAddTreeStaleDestinationIsNotResurrected},
		{"AddTreeStaleDestinationDefaultSelectionDoesNotResurrect", checkAddTreeStaleDestinationDefaultSelectionDoesNotResurrect},
		{"AddRecordsCompressedLooseAuthority", checkAddRecordsCompressedLooseAuthority},
		{"PackedDuplicateIngestRemovesLooseCopy", checkPackedDuplicateIngestRemovesLooseCopy},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			scenario.check(t, postgresIngesterFixture)
		})
	}
}

func TestPhotoFilesystemImportPostgres(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name  string
		check func(*testing.T, ingesterFixtureFactory)
	}{
		{"PhotoImportAmbiguity", checkPhotoImportAmbiguity},
		{"PhotoImportLateJPEGJoinsEarlierRAW", checkPhotoImportLateJPEGJoinsEarlierRAW},
		{"PhotoImportSkipsGroupChangedAfterScan", checkPhotoImportSkipsGroupChangedAfterScan},
		{"PhotoImportAuditedVaultDoesNotCreateDestination", checkPhotoImportAuditedVaultDoesNotCreateDestination},
		{"PhotoImportRunnerHonorsCancelAfterLastGroup", checkPhotoImportRunnerHonorsCancelAfterLastGroup},
		{"PhotoImportRunnerCancelsAndResumes", checkPhotoImportRunnerCancelsAndResumes},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			scenario.check(t, postgresIngesterFixture)
		})
	}
}
