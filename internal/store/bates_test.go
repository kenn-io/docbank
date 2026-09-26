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
	"go.kenn.io/docbank/internal/canonical"
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
		RecipeSHA256: strings.Repeat("d", 64), StartAt: 1, Pages: inputs}
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
	stale := batesRequest(t, ns, snapshot, inputs)
	stale.StartAt = 43
	_, err = s.ReserveBatesRange(t.Context(), stale)
	require.ErrorIs(t, err, ErrBatesReservationConflict, "a start behind the cursor must not reserve")
	next := batesRequest(t, ns, snapshot, inputs)
	next.StartAt = 44
	reserved, err := s.ReserveBatesRange(t.Context(), next)
	require.NoError(t, err)
	require.Equal(t, int64(44), reserved.StartSequence)
}

func TestBatesReserveRequiresTheRecipeStart(t *testing.T) {
	s := newTestStore(t)
	snapshot, inputs := batesFixture(t, s)
	ns, err := s.EnsureBatesNamespace(t.Context(), "OUR", "", 6)
	require.NoError(t, err)
	request := batesRequest(t, ns, snapshot, inputs)
	request.StartAt = 0
	_, err = s.ReserveBatesRange(t.Context(), request)
	require.ErrorIs(t, err, ErrInvalidBatesRequest)
	var allocations int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM bates_allocations`).Scan(&allocations))
	require.Zero(t, allocations, "a cursor-following reservation could never match its recipe")
}

func TestBatesExportPageLimit(t *testing.T) {
	s := newTestStore(t)
	snapshot, inputs := batesFixture(t, s)
	ns, err := s.EnsureBatesNamespace(t.Context(), "OUR", "", 6)
	require.NoError(t, err)
	request := batesRequest(t, ns, snapshot, inputs)
	request.Pages = make([]BatesPageInput, MaxBatesExportPages+1)
	_, err = s.PreviewBatesRange(t.Context(), request)
	require.ErrorIs(t, err, ErrBatesPageLimit)
	_, err = s.ReserveBatesRange(t.Context(), request)
	require.ErrorIs(t, err, ErrBatesPageLimit)
}

func TestNamespaceUniquenessOverflowAndUnverifiedPages(t *testing.T) {
	s := newTestStore(t)
	snapshot, inputs := batesFixture(t, s)
	for _, prefix := range []string{"BAD%", `BAD\`, "BÄD"} {
		_, err := s.EnsureBatesNamespace(t.Context(), prefix, "", 6)
		require.ErrorIs(t, err, ErrInvalidBatesRequest)
	}
	for _, padding := range []int{0, 11} {
		_, err := s.EnsureBatesNamespace(t.Context(), "OUR", "", padding)
		require.ErrorIs(t, err, ErrInvalidBatesRequest)
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
	request.StartAt = 1
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
	_, err = s.PreviewBatesRange(t.Context(), secondRequest)
	require.ErrorIs(t, err, ErrBatesLabelCollision, "preview must show the collision before reserving")
	_, err = s.ReserveBatesRange(t.Context(), secondRequest)
	require.ErrorIs(t, err, ErrBatesLabelCollision)
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
	_, err = s.db.ExecContext(t.Context(), `UPDATE bates_allocations SET state='committed',committed_at='2026-09-21T12:00:00Z' WHERE allocation_id=?`, allocation.AllocationID)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE bates_allocations SET committed_at='2026-09-22T12:00:00Z' WHERE allocation_id=?`, allocation.AllocationID)
	require.Error(t, err, "a commit is final")
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
		request.StartAt = int64(i*3 + 1)
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
		if errs[i] != nil {
			require.ErrorIs(t, errs[i], ErrBatesReservationConflict, "only a start behind the cursor may lose")
			continue
		}
		require.Equal(t, int64(i*3+1), result.StartSequence)
		require.Equal(t, int64(3), result.EndSequence-result.StartSequence+1)
		for n := result.StartSequence; n <= result.EndSequence; n++ {
			require.False(t, seen[n])
			seen[n] = true
		}
	}
	require.NotEmpty(t, seen)
}

func TestBatesLedgerSurvivesMetadataRestore(t *testing.T) {
	s := newTestStore(t)
	snapshot, inputs := batesFixture(t, s)
	ns, err := s.EnsureBatesNamespace(t.Context(), "OUR", "", 6)
	require.NoError(t, err)
	_, err = s.ReserveBatesRange(t.Context(), batesRequest(t, ns, snapshot, inputs))
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &encoded))
	bad := bytes.Replace(encoded.Bytes(), []byte(`"next_sequence":4`), []byte(`"next_sequence":3`), 1)
	require.NotEqual(t, encoded.Bytes(), bad)
	invalid := newTestStore(t)
	err = invalid.ImportMetadata(t.Context(), bytes.NewReader(bad))
	require.ErrorIs(t, err, ErrInvalidBatesLedger)
	require.ErrorContains(t, err, ns.NamespaceID, "a failed restore must name the bad namespace")
	badState := bytes.Replace(encoded.Bytes(), []byte(`"state":"reserved"`), []byte(`"state":"future"`), 1)
	require.NotEqual(t, encoded.Bytes(), badState)
	invalidState := newTestStore(t)
	require.ErrorIs(t, invalidState.ImportMetadata(t.Context(), bytes.NewReader(badState)), ErrInvalidBatesLedger)
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(encoded.Bytes())))
	next := batesRequest(t, ns, snapshot, inputs)
	_, err = restored.ReserveBatesRange(t.Context(), next)
	require.ErrorIs(t, err, ErrBatesReservationConflict, "restored cursor must still cover 1-3")
	next.StartAt = 4
	reserved, err := restored.ReserveBatesRange(t.Context(), next)
	require.NoError(t, err)
	require.Equal(t, int64(4), reserved.StartSequence)
}

func TestBatesArtifactReceiptsMustNameTheSealedSourcePDF(t *testing.T) {
	s := newTestStore(t)
	snapshot, inputs := batesFixture(t, s)
	namespace, err := s.EnsureBatesNamespace(t.Context(), "SRC", "", 6)
	require.NoError(t, err)
	recipe, err := canonical.Marshal(map[string]any{"contract": "bates-stamp/v1"})
	require.NoError(t, err)
	request := batesRequest(t, namespace, snapshot, inputs)
	request.RecipeSHA256 = digestCatalogJSON(recipe)
	allocation, err := s.ReserveBatesRange(t.Context(), request)
	require.NoError(t, err)
	pages := make([]BatesArtifactPage, len(inputs))
	for index, input := range inputs {
		pages[index] = BatesArtifactPage{Ordinal: index + 1, OccurrenceID: input.OccurrenceID,
			SourceBlobSHA256: input.UnstampedSHA256, SourcePage: input.SourcePage,
			OutputPage: index + 1, Label: allocation.Labels[index].Label}
	}
	pages[1].SourceBlobSHA256 = strings.Repeat("e", 64)
	_, err = s.PublishBatesArtifact(t.Context(), BatesArtifactPublication{ArtifactID: allocation.AllocationID,
		AllocationID: allocation.AllocationID, BlobSHA256: strings.Repeat("f", 64), Size: 10,
		PageCount: len(pages), RecipeJSON: recipe, Pages: pages}, BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true})
	require.ErrorIs(t, err, ErrBatesPageCountMismatch)
}

func TestBatesRestoreRejectsCursorPastPadding(t *testing.T) {
	s := newTestStore(t)
	snapshot, inputs := batesFixture(t, s)
	ns, err := s.EnsureBatesNamespace(t.Context(), "PAD", "", 2)
	require.NoError(t, err)
	_, err = s.ReserveBatesRange(t.Context(), batesRequest(t, ns, snapshot, inputs))
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &encoded))
	bad := bytes.Replace(encoded.Bytes(), []byte(`"next_sequence":4`), []byte(`"next_sequence":101`), 1)
	require.NotEqual(t, encoded.Bytes(), bad)
	err = newTestStore(t).ImportMetadata(t.Context(), bytes.NewReader(bad))
	require.ErrorIs(t, err, ErrInvalidBatesLedger)
	require.ErrorContains(t, err, "padding")
}

func TestBatesLedgerValidationRejectsAllocationsOverThePageLimit(t *testing.T) {
	s := newTestStore(t)
	pages := MaxBatesExportPages + 1
	node, err := s.CreateFile(t.Context(), s.RootID(), "large.pdf", fakeHash("c1"), 123, "application/pdf")
	require.NoError(t, err)
	source := document.PageSource{VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: 123}
	frames := make([]document.PageFrameV1, pages)
	for p := range frames {
		frames[p], err = document.NewPDFPageFrame(source, p+1, [4]float64{0, 0, 72, 72}, [4]float64{0, 0, 72, 72}, 0)
		require.NoError(t, err)
	}
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return putPageDocument(t.Context(), tx, document.PageDocumentV1{Contract: document.PageFrameContractV1,
			Source: source, PageCount: pages, Frames: frames})
	}))
	occurrence := strings.Repeat("c", 32)
	id, err := newUUIDv4()
	require.NoError(t, err)
	snapshot, err := s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{SnapshotID: id, Members: []CollectionSnapshotMember{{
		Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID, ContentVersionID: node.CurrentVersionID,
		BlobSHA256: node.BlobHash, Size: 123, FamilyID: occurrence, FamilyOrder: 1, DisplayName: node.Name,
		FrozenFieldsJSON: "{}", DocumentKind: "other", SourcePageCount: pages, SelectedPDFSHA256: node.BlobHash,
	}}})
	require.NoError(t, err)
	ns, err := s.EnsureBatesNamespace(t.Context(), "BIG", "", 6)
	require.NoError(t, err)
	// Normal writes refuse this range, so write the consistent oversized ledger directly.
	allocationID, err := newUUIDv4()
	require.NoError(t, err)
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO bates_allocations(allocation_id,operation_id,namespace_id,snapshot_id,
		request_sha256,recipe_sha256,start_sequence,end_sequence,state,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		allocationID, operationID, ns.NamespaceID, snapshot.SnapshotID, strings.Repeat("a", 64), strings.Repeat("b", 64),
		1, pages, batesAllocationStateReserved, nowRFC3339())
	require.NoError(t, err)
	for page := 1; page <= pages; page++ {
		_, err = s.db.ExecContext(t.Context(), `INSERT INTO bates_page_labels(allocation_id,ordinal,namespace_id,sequence,
			occurrence_id,source_page,output_page,label) VALUES(?,?,?,?,?,?,?,?)`, allocationID, page, ns.NamespaceID,
			page, occurrence, page, page, batesLabel(ns, int64(page)))
		require.NoError(t, err)
	}
	_, err = s.db.ExecContext(t.Context(), `UPDATE bates_namespace_cursors SET next_sequence=? WHERE namespace_id=?`, pages+1, ns.NamespaceID)
	require.NoError(t, err)
	var encoded bytes.Buffer

	// Backup and restore share this ledger validation.
	err = s.ExportMetadata(t.Context(), &encoded)

	require.ErrorIs(t, err, ErrInvalidBatesLedger)
	require.ErrorContains(t, err, allocationID)
}

func TestBatesExplicitPagesMatchASealedPDFWithoutPageDocument(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "native.pdf", fakeHash("d1"), 123, "application/pdf")
	require.NoError(t, err)
	occurrence := strings.Repeat("d", 32)
	id, err := newUUIDv4()
	require.NoError(t, err)
	snapshot, err := s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{SnapshotID: id, Members: []CollectionSnapshotMember{{
		Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID, ContentVersionID: node.CurrentVersionID,
		BlobSHA256: node.BlobHash, Size: 123, FamilyID: occurrence, FamilyOrder: 1, DisplayName: node.Name,
		FrozenFieldsJSON: "{}", DocumentKind: "other", SourcePageCount: 2, SelectedPDFSHA256: node.BlobHash,
		Representations: []CollectionSnapshotRepresentation{{OccurrenceID: occurrence, Role: "native",
			Status: roleAvailable, TextAuthority: "none", ContentVersionID: node.CurrentVersionID,
			BlobSHA256: node.BlobHash, MediaType: "application/pdf", Size: 123, VerifiedPageCount: 2}},
	}}})
	require.NoError(t, err)
	ns, err := s.EnsureBatesNamespace(t.Context(), "NAT", "", 6)
	require.NoError(t, err)
	request := batesRequest(t, ns, snapshot, []BatesPageInput{
		{OccurrenceID: occurrence, UnstampedSHA256: node.BlobHash, SourcePage: 1, VerifiedPageCount: 2},
		{OccurrenceID: occurrence, UnstampedSHA256: node.BlobHash, SourcePage: 2, VerifiedPageCount: 2},
	})

	plan, err := s.PreviewBatesRange(t.Context(), request)

	require.NoError(t, err, "the sealed PDF record verifies the page count when no page document exists")
	require.Len(t, plan.Labels, 2)
}
