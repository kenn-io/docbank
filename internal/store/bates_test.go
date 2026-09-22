package store

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func batesFixture(t *testing.T, s *Store) (CollectionSnapshot, []BatesPageInput) {
	t.Helper()
	var members []CollectionSnapshotMember
	var inputs []BatesPageInput
	for i, count := range []int{2, 1} {
		name := []string{"A.pdf", "B.pdf"}[i]
		node, err := s.CreateFile(t.Context(), s.RootID(), name, fakeHash([]string{"a1", "b1"}[i]), 123, "application/pdf")
		require.NoError(t, err)
		source := document.PageSource{VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: 123}
		frames := make([]document.PageFrameV1, count)
		for p := range frames {
			frames[p], err = document.NewPDFPageFrame(source, p+1, [4]float64{0, 0, 72, 72}, [4]float64{0, 0, 72, 72}, 0)
			require.NoError(t, err)
		}
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			return putPageDocument(t.Context(), tx, document.PageDocumentV1{Contract: document.PageFrameContractV1, Source: source, PageCount: count, Frames: frames})
		}))
		occurrence := strings.Repeat([]string{"a", "b"}[i], 32)
		member := CollectionSnapshotMember{Ordinal: i + 1, OccurrenceID: occurrence, NodeID: node.ID,
			ContentVersionID: node.CurrentVersionID, BlobSHA256: node.BlobHash, Size: 123,
			FamilyID: strings.Repeat("a", 32), FamilyOrder: i + 1, DisplayName: name,
			FrozenFieldsJSON: "{}", DocumentKind: "other", SourcePageCount: count,
			SelectedPDFSHA256: node.BlobHash}
		if i == 1 {
			member.ParentOccurrenceID = members[0].OccurrenceID
		}
		members = append(members, member)
		for p := 1; p <= count; p++ {
			inputs = append(inputs, BatesPageInput{OccurrenceID: occurrence, UnstampedSHA256: node.BlobHash, SourcePage: p, VerifiedPageCount: count})
		}
	}
	id, err := newUUIDv4()
	require.NoError(t, err)
	snapshot, err := s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{SnapshotID: id, Members: members})
	require.NoError(t, err)
	return snapshot, inputs
}

func batesRequest(t *testing.T, ns BatesNamespace, snapshot CollectionSnapshot, inputs []BatesPageInput) BatesPlanRequest {
	t.Helper()
	id, err := newUUIDv4()
	require.NoError(t, err)
	return BatesPlanRequest{OperationID: id, NamespaceID: ns.NamespaceID, SnapshotID: snapshot.SnapshotID,
		RecipeSHA256: strings.Repeat("d", 64), Pages: inputs}
}

func TestBatesRangeContinuesAndRejectsExplicitOverlap(t *testing.T) {
	start, end, err := batesRange(44, 0, 3, 6)
	require.NoError(t, err)
	require.Equal(t, int64(44), start)
	require.Equal(t, int64(46), end)
	_, _, err = batesRange(44, 41, 3, 6)
	require.ErrorIs(t, err, ErrBatesReservationConflict)
	_, _, err = batesRange(1, 9_999_999_999, 2, 10)
	require.ErrorIs(t, err, ErrBatesOverflow)
}

func TestBatesPreviewIsTentativeAndRepeatable(t *testing.T) {
	s := newTestStore(t)
	snapshot, inputs := batesFixture(t, s)
	ns, err := s.EnsureBatesNamespace(t.Context(), "OUR", "", 6)
	require.NoError(t, err)
	request := batesRequest(t, ns, snapshot, inputs)
	request.StartAt = 41
	first, err := s.PreviewBatesRange(t.Context(), request)
	require.NoError(t, err)
	second, err := s.PreviewBatesRange(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, int64(41), first.StartSequence)
	require.Equal(t, []string{"OUR000041", "OUR000042", "OUR000043"}, []string{first.Labels[0].Label, first.Labels[1].Label, first.Labels[2].Label})
	var cursor, allocations int64
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT next_sequence FROM bates_namespace_cursors WHERE namespace_id=?`, ns.NamespaceID).Scan(&cursor))
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM bates_allocations`).Scan(&allocations))
	require.Equal(t, int64(1), cursor)
	require.Zero(t, allocations)
}

func TestABFixtureAllocatesFortyOneToFortyThree(t *testing.T) {
	s := newTestStore(t)
	snapshot, inputs := batesFixture(t, s)
	ns, err := s.EnsureBatesNamespace(t.Context(), "OUR", "", 6)
	require.NoError(t, err)
	request := batesRequest(t, ns, snapshot, inputs)
	request.StartAt = 41
	allocation, err := s.ReserveBatesRange(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "reserved", allocation.State)
	require.Equal(t, []string{"OUR000041", "OUR000042", "OUR000043"}, []string{allocation.Labels[0].Label, allocation.Labels[1].Label, allocation.Labels[2].Label})
	require.Equal(t, inputs[0].OccurrenceID, allocation.Labels[1].OccurrenceID)
	retry, err := s.ReserveBatesRange(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, allocation, retry)
	changed := request
	changed.Pages = append([]BatesPageInput{}, request.Pages...)
	changed.Pages[0].UnstampedSHA256 = strings.Repeat("e", 64)
	_, err = s.ReserveBatesRange(t.Context(), changed)
	require.ErrorIs(t, err, ErrBatesReservationConflict)
	abandoned, err := s.AbandonBatesAllocation(t.Context(), allocation.AllocationID)
	require.NoError(t, err)
	require.Equal(t, "abandoned", abandoned.State)
	next, err := s.ReserveBatesRange(t.Context(), batesRequest(t, ns, snapshot, inputs))
	require.NoError(t, err)
	require.Equal(t, int64(44), next.StartSequence)
	_, err = s.CommitBatesAllocation(t.Context(), next.AllocationID)
	require.ErrorIs(t, err, ErrBatesReservationConflict)
	abandoned, err = s.AbandonBatesAllocation(t.Context(), next.AllocationID)
	require.NoError(t, err)
	require.Equal(t, "abandoned", abandoned.State)
}

func TestNamespaceUniquenessOverflowAndUnverifiedPages(t *testing.T) {
	s := newTestStore(t)
	snapshot, inputs := batesFixture(t, s)
	for _, prefix := range []string{"BAD%", `BAD\`, "BÄD"} {
		_, err := s.EnsureBatesNamespace(t.Context(), prefix, "", 6)
		require.ErrorIs(t, err, ErrBatesReservationConflict)
	}
	ns, err := s.EnsureBatesNamespace(t.Context(), "OUR", "", 6)
	require.NoError(t, err)
	second, err := s.EnsureBatesNamespace(t.Context(), "OUR", "", 8)
	require.ErrorIs(t, err, ErrBatesReservationConflict)
	require.Empty(t, second.NamespaceID)
	request := batesRequest(t, ns, snapshot, inputs)
	request.StartAt = 999999
	_, err = s.ReserveBatesRange(t.Context(), request)
	require.ErrorIs(t, err, ErrBatesOverflow)
	request.StartAt = 0
	request.Pages[0].VerifiedPageCount = 0
	_, err = s.ReserveBatesRange(t.Context(), request)
	require.ErrorIs(t, err, ErrBatesPageCountMismatch)
}

func TestBatesGlobalLabelCollisionRollsBackCursor(t *testing.T) {
	s := newTestStore(t)
	snapshot, inputs := batesFixture(t, s)
	first, err := s.EnsureBatesNamespace(t.Context(), "A", "", 2)
	require.NoError(t, err)
	second, err := s.EnsureBatesNamespace(t.Context(), "A0", "", 1)
	require.NoError(t, err)
	firstRequest := batesRequest(t, first, snapshot, inputs)
	_, err = s.ReserveBatesRange(t.Context(), firstRequest)
	require.NoError(t, err)
	secondRequest := batesRequest(t, second, snapshot, inputs)
	_, err = s.ReserveBatesRange(t.Context(), secondRequest)
	require.ErrorIs(t, err, ErrBatesReservationConflict)
	var cursor int64
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT next_sequence FROM bates_namespace_cursors WHERE namespace_id=?`, second.NamespaceID).Scan(&cursor))
	require.Equal(t, int64(1), cursor)
	secondRequest.StartAt = 4
	allocation, err := s.ReserveBatesRange(t.Context(), secondRequest)
	require.NoError(t, err)
	require.Equal(t, int64(4), allocation.StartSequence)
}

func TestBatesLedgerRejectsMutationAndCursorRewind(t *testing.T) {
	s := newTestStore(t)
	snapshot, inputs := batesFixture(t, s)
	ns, err := s.EnsureBatesNamespace(t.Context(), "OUR", "", 6)
	require.NoError(t, err)
	allocation, err := s.ReserveBatesRange(t.Context(), batesRequest(t, ns, snapshot, inputs))
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE bates_namespace_cursors SET next_sequence=1 WHERE namespace_id=?`, ns.NamespaceID)
	require.Error(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE bates_page_labels SET label='CHANGED' WHERE allocation_id=? AND ordinal=1`, allocation.AllocationID)
	require.Error(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE bates_allocations SET start_sequence=2 WHERE allocation_id=?`, allocation.AllocationID)
	require.Error(t, err)
	_, err = s.CommitBatesAllocation(t.Context(), allocation.AllocationID)
	require.ErrorIs(t, err, ErrBatesReservationConflict)
	_, err = s.AbandonBatesAllocation(t.Context(), allocation.AllocationID)
	require.NoError(t, err)
}

func TestConcurrentBatesReservationsNeverOverlap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bates.db")
	a, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	b, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	snapshot, inputs := batesFixture(t, a)
	ns, err := a.EnsureBatesNamespace(t.Context(), "OUR", "", 6)
	require.NoError(t, err)
	const count = 8
	var results [count]BatesAllocation
	var errs [count]error
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range count {
		request := batesRequest(t, ns, snapshot, inputs)
		wg.Go(func() {
			<-start
			if i%2 == 0 {
				results[i], errs[i] = a.ReserveBatesRange(t.Context(), request)
			} else {
				results[i], errs[i] = b.ReserveBatesRange(t.Context(), request)
			}
		})
	}
	close(start)
	wg.Wait()
	seen := map[int64]bool{}
	for i, result := range results {
		require.NoError(t, errs[i])
		require.Equal(t, int64(3), result.EndSequence-result.StartSequence+1)
		for n := result.StartSequence; n <= result.EndSequence; n++ {
			require.False(t, seen[n])
			seen[n] = true
		}
	}
	require.Len(t, seen, count*3)
}

func TestBatesLedgerSurvivesMetadataRestore(t *testing.T) {
	s := newTestStore(t)
	snapshot, inputs := batesFixture(t, s)
	ns, err := s.EnsureBatesNamespace(t.Context(), "OUR", "", 6)
	require.NoError(t, err)
	allocation, err := s.ReserveBatesRange(t.Context(), batesRequest(t, ns, snapshot, inputs))
	require.NoError(t, err)
	_, err = s.AbandonBatesAllocation(t.Context(), allocation.AllocationID)
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &encoded))
	bad := bytes.Replace(encoded.Bytes(), []byte(`"next_sequence":4`), []byte(`"next_sequence":3`), 1)
	require.NotEqual(t, encoded.Bytes(), bad)
	invalid := newTestStore(t)
	require.ErrorIs(t, invalid.ImportMetadata(t.Context(), bytes.NewReader(bad)), ErrBatesReservationConflict)
	badState := bytes.Replace(encoded.Bytes(), []byte(`"state":"abandoned"`), []byte(`"state":"future"`), 1)
	require.NotEqual(t, encoded.Bytes(), badState)
	invalidState := newTestStore(t)
	require.ErrorIs(t, invalidState.ImportMetadata(t.Context(), bytes.NewReader(badState)), ErrBatesReservationConflict)
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(encoded.Bytes())))
	next, err := restored.ReserveBatesRange(t.Context(), batesRequest(t, ns, snapshot, inputs))
	require.NoError(t, err)
	require.Equal(t, int64(4), next.StartSequence)
}
