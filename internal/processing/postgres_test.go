package processing

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgstoretest"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func postgresProcessingStore(tb testing.TB) *store.Store {
	tb.Helper()
	setup, owner := pgtest.NewDatabase(tb)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	connection := pgstoretest.RuntimeConfig(tb, setup, cfg)
	catalog, err := store.OpenPostgres(tb.Context(), connection, cfg)
	require.NoError(tb, err)
	return catalog
}

func postgresPublicationFixture(tb testing.TB) publicationFixture {
	tb.Helper()
	return publicationFixtureWithStore(tb, tb.TempDir(), postgresProcessingStore(tb))
}

func postgresEmailPipelineFixture(t *testing.T) emailPipelineFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return emailPipelineFixtureWithStore(t, root, postgresProcessingStore(t))
}

func TestProcessingWorkersPostgres(t *testing.T) {
	t.Parallel()
	t.Run("RenditionWorkerPublishesNormalizedBuildAndAllAuthorizedWaiters", func(t *testing.T) {
		t.Parallel()
		checkRenditionWorkerPublishesNormalizedBuildAndAllAuthorizedWaiters(t, postgresPublicationFixture)
	})
	t.Run("RenditionWorkerPersistsAmbiguousOutcomeWithoutResubmission", func(t *testing.T) {
		t.Parallel()
		checkRenditionWorkerPersistsAmbiguousOutcomeWithoutResubmission(t, postgresPublicationFixture)
	})
	t.Run("RenditionWorkerRetainsProviderCheckpointAcrossLocalStagingFailure", func(t *testing.T) {
		t.Parallel()
		checkRenditionWorkerRetainsProviderCheckpointAcrossLocalStagingFailure(t, postgresPublicationFixture)
	})
	t.Run("RenditionWorkerResumesAmbiguousProviderOutcomeWithDurableHandle", func(t *testing.T) {
		t.Parallel()
		checkRenditionWorkerResumesAmbiguousProviderOutcomeWithDurableHandle(t, postgresPublicationFixture)
	})
	t.Run("RenditionWorkerReselectsAuthorizedWaiterWhenBeginConsentChanges", func(t *testing.T) {
		t.Parallel()
		checkRenditionWorkerReselectsAuthorizedWaiterWhenBeginConsentChanges(t, postgresPublicationFixture)
	})
	t.Run("RenditionWorkerFailsClosedWhenConsentIsRevokedBeforeEgress", func(t *testing.T) {
		t.Parallel()
		checkRenditionWorkerFailsClosedWhenConsentIsRevokedBeforeEgress(t, postgresPublicationFixture)
	})
	t.Run("RenditionWorkerReclaimsStagedBuildWithoutCallingProviderAgain", func(t *testing.T) {
		t.Parallel()
		checkRenditionWorkerReclaimsStagedBuildWithoutCallingProviderAgain(t, postgresPublicationFixture)
	})
	t.Run("RenditionWorkerRejectsChangedSourcesBeforeEgress", func(t *testing.T) {
		t.Parallel()
		checkRenditionWorkerRejectsChangedSourcesBeforeEgress(t, postgresPublicationFixture)
	})
	t.Run("RenditionWorkerRejectsResumeAfterSourceChanges", func(t *testing.T) {
		t.Parallel()
		checkRenditionWorkerRejectsResumeAfterSourceChanges(t, postgresPublicationFixture)
	})
	t.Run("MediaEnqueueAuthorizedReturnsBeforeProvider", func(t *testing.T) {
		t.Parallel()
		checkMediaEnqueueAuthorizedReturnsBeforeProvider(t, postgresPublicationFixture)
	})
	t.Run("MediaBackfillPreservesCompletedRevokedInput", func(t *testing.T) {
		t.Parallel()
		checkMediaBackfillPreservesCompletedRevokedInput(t, postgresPublicationFixture)
	})
	t.Run("MediaOperationStateFollowsJob", func(t *testing.T) { t.Parallel(); checkMediaOperationStateFollowsJob(t, postgresPublicationFixture) })
	t.Run("EmailPDFJobRetainsExactReceiptWithoutReplacingBodyHead", func(t *testing.T) {
		t.Parallel()
		checkEmailPDFJobRetainsExactReceiptWithoutReplacingBodyHead(t, postgresEmailPipelineFixture)
	})
	t.Run("ExportEmailPDFPinsRetainedReceiptAndArchive", func(t *testing.T) {
		t.Parallel()
		checkExportEmailPDFPinsRetainedReceiptAndArchive(t, postgresEmailPipelineFixture)
	})
	t.Run("ExportEmailPDFRecipeAcrossDistinctMessages", func(t *testing.T) {
		t.Parallel()
		checkExportEmailPDFRecipeAcrossDistinctMessages(t, postgresEmailPipelineFixture)
	})
}
