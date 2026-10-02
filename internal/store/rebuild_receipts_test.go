package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func peopleBindingEpoch(t *testing.T, s *Store) int64 {
	t.Helper()
	var epoch int64
	require.NoError(t, s.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epoch))
	return epoch
}

func rawRebuildTimes(t *testing.T, s *Store, table, operationID string) (updatedAt string, finishedAt *string) {
	t.Helper()
	require.NoError(t, s.db.QueryRow(`SELECT updated_at,finished_at FROM `+table+` WHERE operation_id=?`, operationID).
		Scan(&updatedAt, &finishedAt))
	return updatedAt, finishedAt
}

func TestDocumentPeopleRebuildConflictKeepsErrExists(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.RebuildDocumentPeople(t.Context(), operationID)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE document_people_builds SET request_sha256=? WHERE operation_id=?`, fakeHash("f1"), operationID)
	require.NoError(t, err)
	epoch := peopleBindingEpoch(t, s)

	_, err = s.RebuildDocumentPeople(t.Context(), operationID)
	require.ErrorIs(t, err, ErrExists)
	require.ErrorContains(t, err, "document people rebuild operation conflicts")
	require.Equal(t, epoch, peopleBindingEpoch(t, s))
}

func TestDocumentPeopleRebuildRollsBackEpochWhenReceiptInsertFails(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.db.Exec(`CREATE TRIGGER reject_document_people_build
		BEFORE INSERT ON document_people_builds BEGIN SELECT RAISE(ABORT, 'reject build'); END`)
	require.NoError(t, err)
	epoch := peopleBindingEpoch(t, s)
	operationID, err := newUUIDv4()
	require.NoError(t, err)

	_, err = s.RebuildDocumentPeople(t.Context(), operationID)
	require.ErrorContains(t, err, "reject build")
	require.Equal(t, epoch, peopleBindingEpoch(t, s), "the epoch bump must roll back with the receipt")
}

func TestPeopleRebuildReplayReturnsRefreshedReceipt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	version := seedDocumentPeopleEvent(t, s, "replay-refreshed.txt", "b1", nil)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	started, err := s.RebuildDocumentPeople(t.Context(), operationID)
	require.NoError(t, err)
	input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
	require.NoError(t, err)
	require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), input, "input_over_limit"))
	require.NoError(t, s.RefreshDocumentPeopleBuilds(t.Context()))
	refreshed, err := s.DocumentPeopleBuild(t.Context(), operationID)
	require.NoError(t, err)
	require.Equal(t, "failed", refreshed.State)
	epoch := peopleBindingEpoch(t, s)

	replayed, err := s.RebuildDocumentPeople(t.Context(), operationID)
	require.NoError(t, err)
	require.Equal(t, refreshed, replayed)
	require.NotEqual(t, started, replayed)
	require.Equal(t, epoch, peopleBindingEpoch(t, s))
}

func TestRebuildReceiptsKeepFinishedAtConventions(t *testing.T) {
	t.Parallel()
	t.Run("events", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		node, err := s.CreateFile(t.Context(), s.RootID(), "conventions.txt", fakeHash("c1"), 1, "text/plain")
		require.NoError(t, err)
		build, err := s.StartDocumentEventRebuild(t.Context(), "40000000-0000-4000-8000-000000000001", fakeHash("c1"))
		require.NoError(t, err)
		updatedAt, finishedAt := rawRebuildTimes(t, s, "document_event_builds", build.OperationID)
		require.Nil(t, finishedAt)

		require.NoError(t, s.RefreshDocumentEventBuilds(t.Context()))
		unchanged, finishedAt := rawRebuildTimes(t, s, "document_event_builds", build.OperationID)
		require.Equal(t, updatedAt, unchanged, "a no-change refresh leaves the receipt untouched")
		require.Nil(t, finishedAt)

		target := requireDocumentEventTarget(t, s, node.CurrentVersionID)
		require.NoError(t, s.RecordDocumentEventAttempt(t.Context(), target,
			requireDocumentEventInputsSHA256(t, s, target), "unavailable", []byte(`[{"code":"unsupported"}]`)))
		require.NoError(t, s.RefreshDocumentEventBuilds(t.Context()))
		_, finishedAt = rawRebuildTimes(t, s, "document_event_builds", build.OperationID)
		require.NotNil(t, finishedAt)
		require.NotEmpty(t, *finishedAt)
	})
	t.Run("people", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		version := seedDocumentPeopleEvent(t, s, "conventions.txt", "c2", nil)
		operationID, err := newUUIDv4()
		require.NoError(t, err)
		_, err = s.RebuildDocumentPeople(t.Context(), operationID)
		require.NoError(t, err)
		updatedAt, finishedAt := rawRebuildTimes(t, s, "document_people_builds", operationID)
		require.NotNil(t, finishedAt)
		require.Empty(t, *finishedAt)

		require.NoError(t, s.RefreshDocumentPeopleBuilds(t.Context()))
		unchanged, finishedAt := rawRebuildTimes(t, s, "document_people_builds", operationID)
		require.Equal(t, updatedAt, unchanged, "a no-change refresh leaves the receipt untouched")
		require.NotNil(t, finishedAt)
		require.Empty(t, *finishedAt)

		input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
		require.NoError(t, err)
		require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), input, "input_over_limit"))
		require.NoError(t, s.RefreshDocumentPeopleBuilds(t.Context()))
		_, finishedAt = rawRebuildTimes(t, s, "document_people_builds", operationID)
		require.NotNil(t, finishedAt)
		require.NotEmpty(t, *finishedAt)
	})
}

func TestRebuildReceiptsKeepProgressRules(t *testing.T) {
	t.Parallel()
	t.Run("events", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		var targets []DocumentEventTarget
		for i, seed := range []string{"d1", "d2", "d3"} {
			node, err := s.CreateFile(t.Context(), s.RootID(), []string{"indexed.txt", "failed.txt", "unavailable.txt"}[i], fakeHash(seed), 1, "text/plain")
			require.NoError(t, err)
			targets = append(targets, DocumentEventTarget{ContentVersionID: node.CurrentVersionID})
		}
		build, err := s.StartDocumentEventRebuild(t.Context(), "40000000-0000-4000-8000-000000000002", fakeHash("d1"))
		require.NoError(t, err)
		for i := range targets {
			targets[i] = requireDocumentEventTarget(t, s, targets[i].ContentVersionID)
		}
		_, err = s.PublishDocumentEvents(t.Context(), targets[0], DocumentEventsDeriverFingerprint,
			requireDocumentEventInputsSHA256(t, s, targets[0]),
			mustMarshalDocumentEvents(t, documentEventRecord(t, s.VaultID(), targets[0].ContentVersionID, "d")))
		require.NoError(t, err)
		require.NoError(t, s.RecordDocumentEventAttempt(t.Context(), targets[1],
			requireDocumentEventInputsSHA256(t, s, targets[1]), "failed", []byte(`[{"code":"unsupported"}]`)))
		require.NoError(t, s.RecordDocumentEventAttempt(t.Context(), targets[2],
			requireDocumentEventInputsSHA256(t, s, targets[2]), "unavailable", []byte(`[{"code":"unsupported"}]`)))
		require.NoError(t, s.RefreshDocumentEventBuilds(t.Context()))
		build, err = s.DocumentEventBuild(t.Context(), build.OperationID)
		require.NoError(t, err)
		require.Equal(t, "failed", build.State)
		require.Equal(t, int64(3), build.Scanned)
		require.Equal(t, int64(1), build.Published)
		require.Equal(t, int64(1), build.Failed)
		require.Equal(t, int64(1), build.Unavailable)
	})
	t.Run("people unavailable", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		published := seedDocumentPeopleEvent(t, s, "published.txt", "e1", nil)
		unavailable := seedDocumentPeopleEvent(t, s, "unavailable.txt", "e2", nil)
		operationID, err := newUUIDv4()
		require.NoError(t, err)
		_, err = s.RebuildDocumentPeople(t.Context(), operationID)
		require.NoError(t, err)
		input, err := s.PrepareDocumentPeopleInputs(t.Context(), published.ID)
		require.NoError(t, err)
		_, err = s.PublishDocumentPeople(t.Context(), documentPeoplePublicationForInput(t, input))
		require.NoError(t, err)
		input, err = s.PrepareDocumentPeopleInputs(t.Context(), unavailable.ID)
		require.NoError(t, err)
		require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), input, "input_over_limit"))
		require.NoError(t, s.RefreshDocumentPeopleBuilds(t.Context()))
		build, err := s.DocumentPeopleBuild(t.Context(), operationID)
		require.NoError(t, err)
		require.Equal(t, "failed", build.State)
		require.Equal(t, int64(2), build.Scanned)
		require.Equal(t, int64(1), build.Published)
		require.Equal(t, int64(1), build.Failed, "people fold unavailable heads into failed")
	})
	t.Run("people retrying", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		version := seedDocumentPeopleEvent(t, s, "retrying.txt", "e3", nil)
		operationID, err := newUUIDv4()
		require.NoError(t, err)
		_, err = s.RebuildDocumentPeople(t.Context(), operationID)
		require.NoError(t, err)
		input, err := s.PrepareDocumentPeopleInputs(t.Context(), version.ID)
		require.NoError(t, err)
		require.NoError(t, s.MarkDocumentPeopleFailed(t.Context(), input, "derivation_failed"))
		require.NoError(t, s.RefreshDocumentPeopleBuilds(t.Context()))
		build, err := s.DocumentPeopleBuild(t.Context(), operationID)
		require.NoError(t, err)
		require.Equal(t, "running", build.State, "a retrying failed head keeps the build open")
		require.Equal(t, int64(1), build.Failed)
	})
}

func TestRebuildReceiptsRejectCorruptRows(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	peopleID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.RebuildDocumentPeople(t.Context(), peopleID)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE document_people_builds SET state='completed',finished_at='' WHERE operation_id=?`, peopleID)
	require.NoError(t, err)
	_, err = s.DocumentPeopleBuild(t.Context(), peopleID)
	require.ErrorIs(t, err, ErrDocumentPeopleCorrupt)

	events, err := s.StartDocumentEventRebuild(t.Context(), "40000000-0000-4000-8000-000000000003", fakeHash("a1"))
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE document_event_builds SET scanned=scanned+1 WHERE operation_id=?`, events.OperationID)
	require.NoError(t, err)
	_, err = s.DocumentEventBuild(t.Context(), events.OperationID)
	require.ErrorIs(t, err, errDocumentEventBuildInvalid)
}
