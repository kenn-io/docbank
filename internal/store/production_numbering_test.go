package store

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func productionNumberingRequest(
	jobID string,
	namespace BatesNamespace,
	snapshot CollectionSnapshot,
	pages []BatesPageInput,
) ProductionNumberingRequest {
	return ProductionNumberingRequest{
		JobID:        jobID,
		NamespaceID:  namespace.NamespaceID,
		SnapshotID:   snapshot.SnapshotID,
		RecipeSHA256: strings.Repeat("d", 64),
		Pages:        pages,
	}
}

func TestProductionNumberingBindsTwoJobsAndReplaysByJobID(t *testing.T) {
	s := newTestStore(t)
	snapshot, pages := batesFixture(t, s)
	namespace, err := s.EnsureBatesNamespace(t.Context(), "PROD", "", 6)
	require.NoError(t, err)
	firstJob, err := newUUIDv4()
	require.NoError(t, err)
	secondJob, err := newUUIDv4()
	require.NoError(t, err)

	first, err := s.ReserveProductionNumbers(t.Context(), productionNumberingRequest(firstJob, namespace, snapshot, pages))
	require.NoError(t, err)
	second, err := s.ReserveProductionNumbers(t.Context(), productionNumberingRequest(secondJob, namespace, snapshot, pages))
	require.NoError(t, err)
	require.Equal(t, int64(1), first.StartSequence)
	require.Equal(t, int64(4), second.StartSequence)

	replay, err := s.ReserveProductionNumbers(t.Context(), productionNumberingRequest(firstJob, namespace, snapshot, pages))
	require.NoError(t, err)
	require.Equal(t, first, replay)
	retained, err := s.ProductionNumberingForJob(t.Context(), firstJob)
	require.NoError(t, err)
	require.Equal(t, first, retained)
}

func TestProductionNumberingRejectsChangedPayloadForJob(t *testing.T) {
	s := newTestStore(t)
	snapshot, pages := batesFixture(t, s)
	namespace, err := s.EnsureBatesNamespace(t.Context(), "PROD", "", 6)
	require.NoError(t, err)
	jobID, err := newUUIDv4()
	require.NoError(t, err)
	request := productionNumberingRequest(jobID, namespace, snapshot, pages)
	_, err = s.ReserveProductionNumbers(t.Context(), request)
	require.NoError(t, err)

	request.RecipeSHA256 = strings.Repeat("e", 64)
	_, err = s.ReserveProductionNumbers(t.Context(), request)
	require.ErrorIs(t, err, ErrBatesReservationConflict)
}

func TestProductionNumberingLookupReportsUnreservedJobWithoutAllocating(t *testing.T) {
	s := newTestStore(t)
	snapshot, pages := batesFixture(t, s)
	namespace, err := s.EnsureBatesNamespace(t.Context(), "PROD", "", 6)
	require.NoError(t, err)
	firstJob, err := newUUIDv4()
	require.NoError(t, err)
	first, err := s.ReserveProductionNumbers(t.Context(), productionNumberingRequest(firstJob, namespace, snapshot, pages))
	require.NoError(t, err)
	unknownJob, err := newUUIDv4()
	require.NoError(t, err)

	_, err = s.ProductionNumberingForJob(t.Context(), unknownJob)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.ProductionNumberingForJob(t.Context(), "not-a-uuid")
	require.ErrorIs(t, err, ErrNotFound)

	second, err := s.ReserveProductionNumbers(t.Context(), productionNumberingRequest(unknownJob, namespace, snapshot, pages))
	require.NoError(t, err)
	require.Equal(t, first.EndSequence+1, second.StartSequence)
}

func TestProductionNumberingConcurrentJobsNeverOverlap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "production-numbering.db")
	firstStore, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, firstStore.Close()) })
	secondStore, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, secondStore.Close()) })
	snapshot, pages := batesFixture(t, firstStore)
	namespace, err := firstStore.EnsureBatesNamespace(t.Context(), "PROD", "", 6)
	require.NoError(t, err)

	const jobs = 8
	var allocations [jobs]BatesAllocation
	var reservationErrors [jobs]error
	start := make(chan struct{})
	var group sync.WaitGroup
	for index := range jobs {
		jobID, idErr := newUUIDv4()
		require.NoError(t, idErr)
		request := productionNumberingRequest(jobID, namespace, snapshot, pages)
		group.Go(func() {
			<-start
			catalog := firstStore
			if index%2 != 0 {
				catalog = secondStore
			}
			allocations[index], reservationErrors[index] = catalog.ReserveProductionNumbers(t.Context(), request)
		})
	}
	close(start)
	group.Wait()

	seen := make(map[int64]bool, jobs*len(pages))
	for index, allocation := range allocations {
		require.NoError(t, reservationErrors[index])
		for sequence := allocation.StartSequence; sequence <= allocation.EndSequence; sequence++ {
			require.False(t, seen[sequence])
			seen[sequence] = true
		}
	}
	require.Len(t, seen, jobs*len(pages))
}

func TestProductionNumberingAbandonmentDoesNotReuseNumbers(t *testing.T) {
	s := newTestStore(t)
	snapshot, pages := batesFixture(t, s)
	namespace, err := s.EnsureBatesNamespace(t.Context(), "PROD", "", 6)
	require.NoError(t, err)
	firstJob, err := newUUIDv4()
	require.NoError(t, err)
	firstRequest := productionNumberingRequest(firstJob, namespace, snapshot, pages)
	first, err := s.ReserveProductionNumbers(t.Context(), firstRequest)
	require.NoError(t, err)
	abandoned, err := s.AbandonProductionNumbers(t.Context(), firstJob)
	require.NoError(t, err)
	require.Equal(t, "abandoned", abandoned.State)

	replay, err := s.ReserveProductionNumbers(t.Context(), firstRequest)
	require.NoError(t, err)
	require.Equal(t, abandoned, replay)
	secondJob, err := newUUIDv4()
	require.NoError(t, err)
	second, err := s.ReserveProductionNumbers(t.Context(), productionNumberingRequest(secondJob, namespace, snapshot, pages))
	require.NoError(t, err)
	require.Equal(t, first.EndSequence+1, second.StartSequence)
}

func TestProductionNumberingLookupSurvivesStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "production-numbering-reopen.db")
	s, err := Open(path)
	require.NoError(t, err)
	snapshot, pages := batesFixture(t, s)
	namespace, err := s.EnsureBatesNamespace(t.Context(), "PROD", "", 6)
	require.NoError(t, err)
	jobID, err := newUUIDv4()
	require.NoError(t, err)
	request := productionNumberingRequest(jobID, namespace, snapshot, pages)
	reserved, err := s.ReserveProductionNumbers(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	reopened, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	retained, err := reopened.ProductionNumberingForJob(t.Context(), jobID)
	require.NoError(t, err)
	require.Equal(t, reserved, retained)
	replayed, err := reopened.ReserveProductionNumbers(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, reserved, replayed)
}
