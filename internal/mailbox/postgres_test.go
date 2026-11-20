package mailbox

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgstoretest"
	"go.kenn.io/docbank/internal/store"
)

func mailboxPostgresFixture(t *testing.T) *Service {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	_, cfg, runtime := pgstoretest.NewDatabase(t)
	s, err := store.OpenPostgres(t.Context(), runtime, cfg, dir)
	require.NoError(t, err)
	return mailboxFixtureWithStore(t, s, dir)
}

func TestMailboxImportsPostgres(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name string
		run  func(*testing.T, *Service)
	}{
		{"ContainerVerifiesPayloadBeforeSealAndRandomAccess", checkContainerVerifiesPayloadBeforeSealAndRandomAccess},
		{"FailedMailboxTransferIsCollected", checkFailedMailboxTransferIsCollected},
		{"MailboxImportCompleteDistinctOccurrencesAndContinuation", checkMailboxImportCompleteDistinctOccurrencesAndContinuation},
		{"MailboxMalformedEntryReportsPartialCommittedWork", checkMailboxMalformedEntryReportsPartialCommittedWork},
		{"MailboxEmptyZIPEntriesDoNotHideMessages", checkMailboxEmptyZIPEntriesDoNotHideMessages},
		{"MailboxEmptyZIPArchiveCompletes", checkMailboxEmptyZIPArchiveCompletes},
		{"MailboxPreviewSamplesBeforeWorkerVerifiesWholeZIP", checkMailboxPreviewSamplesBeforeWorkerVerifiesWholeZIP},
		{"MailboxMalformedHeadersRetainDecoderEvidenceAndLabels", checkMailboxMalformedHeadersRetainDecoderEvidenceAndLabels},
		{"MailboxWorkerLogsJobFailureAndProcessesNextJob", checkMailboxWorkerLogsJobFailureAndProcessesNextJob},
		{"CancellationFencesPublicationUntilResume", checkMailboxCancellationFencesPublicationUntilResume},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			scenario.run(t, mailboxPostgresFixture(t))
		})
	}
	t.Run("ContinuationAtEntryEnd", func(t *testing.T) {
		t.Parallel()
		checkMailboxContinuationAtEntryEndDoesNotRespoolCommittedMessages(t, mailboxPostgresFixture)
	})
}
