package store

import (
	"bytes"
	"database/sql"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

func TestMediaRetryAdmissionOrderWithEqualClockTimes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		retained, err := s.RetainSuppliedMedia(ctx, suppliedMediaPublicationFixture(t, s))
		require.NoError(t, err)
		sourceID, sourceVersionID := retained.SourceID, retained.SourceVersionID
		older := "00000000-0000-4000-8000-000000000002"
		newer := "00000000-0000-4000-8000-000000000001"
		for _, id := range []string{older, newer} {
			receipt := MediaPublicationReceipt{VaultUID: s.VaultID(), SourceID: sourceID,
				SourceVersionID: sourceVersionID,
				OperationID:     id, OperationState: "queued", CoverageState: "pending", ProcessingProfile: "speech"}
			_, err := s.QueueMediaRetry(ctx, MediaOperation{ID: id, Principal: "operator", Verb: "retry_media",
				SourceID: sourceID, RequestSHA256: strings.Repeat("b", 64)}, receipt)
			require.NoError(t, err)
		}
		_, err = s.FinishMediaProcessing(ctx, older, "operator", true)
		require.NoError(t, err)
		current, err := s.latestMediaProcessingReceiptForVersion(ctx, "operator", sourceID, sourceVersionID, false)
		require.NoError(t, err)
		require.Equal(t, newer, current.OperationID)
		coverage, err := s.latestMediaProcessingReceiptForVersion(ctx, "operator", sourceID, sourceVersionID, true)
		require.NoError(t, err)
		require.Equal(t, older, coverage.OperationID)

		var exported bytes.Buffer
		require.NoError(t, s.ExportMetadata(ctx, &exported))
		restored := newTestStore(t)
		require.NoError(t, restored.ImportMetadata(ctx, &exported))
		current, err = restored.latestMediaProcessingReceiptForVersion(ctx, "operator", sourceID, sourceVersionID, false)
		require.NoError(t, err)
		require.Equal(t, newer, current.OperationID)
		coverage, err = restored.latestMediaProcessingReceiptForVersion(ctx, "operator", sourceID, sourceVersionID, true)
		require.NoError(t, err)
		require.Equal(t, older, coverage.OperationID)
	})
}

func TestMediaOperationReplaysWithoutRepeatingMutation(t *testing.T) {
	s := newTestStore(t)
	calls := 0
	op := MediaOperation{
		ID: "00000000-0000-4000-8000-000000000001", Principal: "operator",
		Verb: "declare_occurrence", RequestSHA256: strings.Repeat("a", 64),
	}
	mutate := func(*sql.Tx) (string, error) {
		calls++
		return `{"occurrence_id":"one"}`, nil
	}
	a, err := s.withMediaOperation(t.Context(), op, mutate)
	require.NoError(t, err)
	b, err := s.withMediaOperation(t.Context(), op, mutate)
	require.NoError(t, err)
	require.Equal(t, a, b)
	require.Equal(t, 1, calls)
	wrongOwner := op
	wrongOwner.Principal = "other"
	_, err = s.MediaOperationReceipt(t.Context(), wrongOwner)
	require.ErrorIs(t, err, ErrNotFound)
	op.RequestSHA256 = strings.Repeat("b", 64)
	_, err = s.withMediaOperation(t.Context(), op, mutate)
	require.ErrorIs(t, err, ErrMediaOperationConflict)
}

func TestMediaOperationRollsBackMutationBeforeInvalidReceipt(t *testing.T) {
	s := newTestStore(t)
	op := MediaOperation{
		ID: "00000000-0000-4000-8000-000000000002", Principal: "operator",
		Verb: "submit_supplied_media", RequestSHA256: strings.Repeat("b", 64),
	}
	_, err := s.withMediaOperation(t.Context(), op, func(tx *sql.Tx) (string, error) {
		_, err := tx.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
			strings.Repeat("c", 64), nowRFC3339())
		require.NoError(t, err)
		return `{`, nil
	})
	require.Error(t, err)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM media_sources WHERE source_id='source'`).Scan(&count))
	require.Zero(t, count)

	receipt, err := s.withMediaOperation(t.Context(), op, func(tx *sql.Tx) (string, error) {
		_, err := tx.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`,
			strings.Repeat("c", 64), nowRFC3339())
		return `{"source_id":"source"}`, err
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"source_id":"source"}`, receipt)
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM media_sources WHERE source_id='source'`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestMediaOperationConcurrentReplayRunsMutationOnce(t *testing.T) {
	s := newTestStore(t)
	op := MediaOperation{
		ID: "00000000-0000-4000-8000-000000000003", Principal: "operator",
		Verb: "retry_media", RequestSHA256: strings.Repeat("d", 64),
	}
	var calls atomic.Int64
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			_, err := s.withMediaOperation(t.Context(), op, func(*sql.Tx) (string, error) {
				calls.Add(1)
				return `{}`, nil
			})
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int64(1), calls.Load())
}

func TestMediaOperationRespectsAuditedVaultGuard(t *testing.T) {
	s := newTestStore(t)
	seedInitialAuditAuthority(t, s, s.RootID())
	op := MediaOperation{
		ID: "00000000-0000-4000-8000-000000000004", Principal: "operator",
		Verb: "declare_occurrence", RequestSHA256: strings.Repeat("e", 64),
	}
	_, err := s.withMediaOperation(t.Context(), op, func(*sql.Tx) (string, error) { return `{}`, nil })
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
}

func TestQueuedMediaOperationPersistsAdmissionBeforeWorkerExecution(t *testing.T) {
	s := newTestStore(t)
	op := MediaOperation{
		ID: "00000000-0000-4000-8000-000000000005", Principal: "operator",
		Verb: "submit_remote_recording", RequestSHA256: strings.Repeat("f", 64),
	}
	calls := 0
	first, err := s.withQueuedMediaOperation(t.Context(), op, func(*sql.Tx) (string, error) {
		calls++
		return `{"outcome":"queued"}`, nil
	})
	require.NoError(t, err)
	second, err := s.withQueuedMediaOperation(t.Context(), op, func(*sql.Tx) (string, error) {
		calls++
		return `{}`, nil
	})
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, 1, calls)
	var state string
	require.NoError(t, s.db.QueryRow(`SELECT state FROM media_operations WHERE operation_id=?`, op.ID).Scan(&state))
	require.Equal(t, mediaOperationQueued, state)
}

func TestMediaContinuationsPrioritizeUnenqueuedWorkAndFilterPrincipal(t *testing.T) {
	s := newTestStore(t)
	_, err := s.db.Exec(`INSERT INTO media_sources VALUES(?, 'supplied_media','','',?,?)`,
		strings.Repeat("a", 64), strings.Repeat("a", 64), nowRFC3339())
	require.NoError(t, err)
	queue := func(id, principal, jobID string) {
		t.Helper()
		receipt := MediaPublicationReceipt{VaultUID: s.VaultID(), SourceID: strings.Repeat("a", 64),
			SourceVersionID: "source-version", ContentVersionID: "content-version", OccurrenceID: "occurrence",
			OperationID: id, JobID: jobID, OperationState: mediaOperationQueued, CoverageState: "pending",
			ProcessingProfile: "supplied-transcript", ProcessingPrincipal: principal}
		_, err := s.QueueMediaRetry(t.Context(), MediaOperation{ID: id, Principal: principal,
			Verb: "retry_media", RequestSHA256: strings.Repeat("b", 64), SourceID: receipt.SourceID}, receipt)
		require.NoError(t, err)
	}
	queue("00000000-0000-4000-8000-000000000051", "operator", strings.Repeat("c", 64))
	queue("00000000-0000-4000-8000-000000000052", "operator", "")
	queue("00000000-0000-4000-8000-000000000053", "other", "")
	items, err := s.MediaProcessingContinuations(t.Context(), 10, "operator")
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, "00000000-0000-4000-8000-000000000052", items[0].OperationID)
	require.Equal(t, "00000000-0000-4000-8000-000000000051", items[1].OperationID)
}
