package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBatesNamespaceDetailCursor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	snapshot, inputs := batesFixture(t, s)
	members, err := s.SnapshotMembers(t.Context(), snapshot.SnapshotID, 0, 1)
	require.NoError(t, err)
	id, err := newUUIDv4()
	require.NoError(t, err)
	snapshot, err = s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{SnapshotID: id, Members: members})
	require.NoError(t, err)
	ns, err := s.EnsureBatesNamespace(t.Context(), "DETAIL", "", 1)
	require.NoError(t, err)
	read := func(want int64) {
		t.Helper()
		for range 2 {
			detail, err := s.BatesNamespaceDetail(t.Context(), ns.NamespaceID)
			require.NoError(t, err)
			require.Equal(t, ns, detail.Namespace)
			require.Equal(t, want, detail.NextSequence)
		}
	}
	read(1)
	request := batesRequest(t, ns, snapshot, inputs[:2])
	_, err = s.ReserveBatesRange(t.Context(), request)
	require.NoError(t, err)
	read(3)
	request = batesRequest(t, ns, snapshot, inputs[:2])
	request.StartAt = 3
	_, err = s.ReserveBatesRange(t.Context(), request)
	require.NoError(t, err)
	read(5)
	request = batesRequest(t, ns, snapshot, inputs[:2])
	request.StartAt = 8
	_, err = s.ReserveBatesRange(t.Context(), request)
	require.NoError(t, err)
	read(10)
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM bates_allocations`).Scan(&count))
	require.Equal(t, 3, count)
	_, err = s.BatesNamespaceDetail(t.Context(), "00000000-0000-4000-8000-000000000001")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.db.ExecContext(t.Context(), `DELETE FROM bates_namespace_cursors WHERE namespace_id=?`, ns.NamespaceID)
	require.NoError(t, err)
	_, err = s.BatesNamespaceDetail(t.Context(), ns.NamespaceID)
	require.ErrorIs(t, err, ErrInvalidBatesLedger)
}
