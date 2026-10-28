package store

import (
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
)

func numberedSnapshotFixture(t *testing.T, s *Store) (SnapshotSealRequest, BatesPlanRequest) {
	t.Helper()
	node, err := s.CreateFile(t.Context(), s.RootID(), "original.pdf", fakeHash("a1"), 123, "application/pdf")
	require.NoError(t, err)
	ns, err := s.EnsureBatesNamespace(t.Context(), "QUAL", "", 6)
	require.NoError(t, err)
	occurrence := strings.Repeat("a", 32)
	snapshot := SnapshotSealRequest{SnapshotID: uuid.New().String(), Members: []CollectionSnapshotMember{{
		Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID, ContentVersionID: node.CurrentVersionID,
		BlobSHA256: node.BlobHash, Size: 123, FamilyID: occurrence, FamilyOrder: 1, DisplayName: node.Name,
		FrozenFieldsJSON: "{}", DocumentKind: "other", SourcePageCount: 1, SelectedSourcePages: []int{1},
		SelectedPDFSHA256: node.BlobHash, Representations: []CollectionSnapshotRepresentation{{
			OccurrenceID: occurrence, Role: "native", Status: "available", TextAuthority: "none",
			ContentVersionID: node.CurrentVersionID, BlobSHA256: node.BlobHash, Size: 123,
			MediaType: "application/pdf", VerifiedPageCount: 1}},
	}}}
	r := BatesPlanRequest{OperationID: uuid.New().String(), NamespaceID: ns.NamespaceID,
		SnapshotID: snapshot.SnapshotID, RecipeSHA256: fakeHash("b1"), StartAt: 1,
		Pages: []BatesPageInput{{OccurrenceID: occurrence, UnstampedSHA256: node.BlobHash,
			SourcePage: 1, VerifiedPageCount: 1}}}
	return snapshot, r
}

func TestBatesSnapshotReservationAtomic(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	snapshot, request := numberedSnapshotFixture(t, s)
	_, err := s.db.ExecContext(t.Context(), `CREATE TRIGGER refuse_test_allocation BEFORE INSERT ON bates_allocations
		BEGIN SELECT RAISE(ABORT, 'injected allocation failure'); END`)
	require.NoError(t, err)
	_, err = s.ReserveSnapshotBatesRange(t.Context(), snapshot, request)
	require.ErrorContains(t, err, "injected allocation failure")
	path := s.path
	require.NoError(t, s.Close())
	s, err = Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	var count, cursor int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM collection_snapshots`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT next_sequence FROM bates_namespace_cursors`).Scan(&cursor))
	require.Equal(t, 1, cursor)
	_, err = s.db.ExecContext(t.Context(), `DROP TRIGGER refuse_test_allocation`)
	require.NoError(t, err)
	first, err := s.ReserveSnapshotBatesRange(t.Context(), snapshot, request)
	require.NoError(t, err)
	request.OperationID, request.SnapshotID = uuid.New().String(), uuid.New().String()
	snapshot.SnapshotID = request.SnapshotID
	replay, err := s.ReserveSnapshotBatesRange(t.Context(), snapshot, request)
	require.NoError(t, err)
	require.Equal(t, first, replay)
	request.RecipeSHA256 = fakeHash("c1")
	_, err = s.ReserveSnapshotBatesRange(t.Context(), snapshot, request)
	require.ErrorIs(t, err, ErrBatesReservationConflict)
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM collection_snapshots`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestBatesSnapshotReservationConcurrent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	other, err := Open(s.path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	stores := []*Store{s, other}
	snapshot, request := numberedSnapshotFixture(t, s)
	var group sync.WaitGroup
	results := make([]BatesAllocation, 4)
	errs := make([]error, 4)
	for i := range results {
		group.Go(func() {
			candidate, operation := snapshot, request
			operation.SnapshotID, operation.OperationID = uuid.New().String(), uuid.New().String()
			candidate.SnapshotID = operation.SnapshotID
			results[i], errs[i] = stores[i%len(stores)].ReserveSnapshotBatesRange(t.Context(), candidate, operation)
		})
	}
	group.Wait()
	for i := range results {
		require.NoError(t, errs[i])
		require.Equal(t, results[0], results[i])
	}
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM collection_snapshots`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestBatesSnapshotReservationRechecksLiveSource(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	snapshot, request := numberedSnapshotFixture(t, s)
	_, _, err := s.Trash(t.Context(), snapshot.Members[0].NodeID, UnconditionalRev)
	require.NoError(t, err)
	_, err = s.ReserveSnapshotBatesRange(t.Context(), snapshot, request)
	require.ErrorIs(t, err, ErrNotFound)
	var snapshots, allocations int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM collection_snapshots`).Scan(&snapshots))
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM bates_allocations`).Scan(&allocations))
	require.Zero(t, snapshots)
	require.Zero(t, allocations)
}
