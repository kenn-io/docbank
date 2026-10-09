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
	t.Parallel()
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
		item, err := s.MediaSourceVersion(ctx, "operator", sourceID, sourceVersionID)
		require.NoError(t, err)
		receipts := item.ProcessingReceipts
		require.Equal(t, []string{newer, older}, []string{receipts[0].OperationID, receipts[1].OperationID})

		var exported bytes.Buffer
		require.NoError(t, s.ExportMetadata(ctx, &exported))
		restored := newTestStore(t)
		require.NoError(t, restored.ImportMetadata(ctx, &exported))
		item, err = restored.MediaSourceVersion(ctx, "operator", sourceID, sourceVersionID)
		require.NoError(t, err)
		receipts = item.ProcessingReceipts
		require.Equal(t, []string{newer, older}, []string{receipts[0].OperationID, receipts[1].OperationID})
	})
}

func TestMediaOperationReplaysWithoutRepeatingMutation(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	s := newTestStore(t)
	seedInitialAuditAuthority(t, s, s.RootID())
	op := MediaOperation{
		ID: "00000000-0000-4000-8000-000000000004", Principal: "operator",
		Verb: "declare_occurrence", RequestSHA256: strings.Repeat("e", 64),
	}
	_, err := s.withMediaOperation(t.Context(), op, func(*sql.Tx) (string, error) { return `{}`, nil })
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
}

func TestRemoteRecordingReceiptScopesPrincipalAndVerb(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	_, operation := remoteStoreReference(t, s, testSHA256([]byte("receipt-source")),
		"00000000-0000-4000-8000-000000000901", "receipt-occurrence")
	stored, err := s.MediaOperationReceipt(ctx, operation)
	require.NoError(t, err)

	got, err := s.RemoteRecordingReceipt(ctx, operation.Principal, operation.ID)
	require.NoError(t, err)
	require.Equal(t, stored, got)

	_, err = s.RemoteRecordingReceipt(ctx, "operator:other", operation.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.RemoteRecordingReceipt(ctx, operation.Principal, "00000000-0000-4000-8000-000000000902")
	require.ErrorIs(t, err, ErrNotFound)
	supplied, err := s.RetainSuppliedMedia(ctx, suppliedMediaPublicationFixture(t, s))
	require.NoError(t, err)
	_, err = s.RemoteRecordingReceipt(ctx, "operator", supplied.OperationID)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = s.RemoteRecordingReceipt(ctx, operation.Principal, "not-a-uuid")
	require.ErrorContains(t, err, "invalid")
	require.NotErrorIs(t, err, ErrNotFound)
}
