package store

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

func TestSnapshotMemberHashUsesNumericNodeOrderAndDistinctMembership(t *testing.T) {
	const first = "00000000-0000-4000-8000-000000000001"
	const second = "00000000-0000-4000-8000-000000000002"
	members := []CollectionSnapshotMember{
		{NodeID: 10, ContentVersionID: first},
		{NodeID: 2, ContentVersionID: first},
		{NodeID: 2, ContentVersionID: first},
	}
	want := sha256.Sum256([]byte("2:" + first + "\n10:" + first + "\n"))
	got, err := collectionSnapshotMemberHash(members)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(want[:]), got)

	members[2].ContentVersionID = second
	_, err = collectionSnapshotMemberHash(members)
	require.ErrorIs(t, err, ErrPackageConflict)
}

func TestSealCollectionSnapshotFreezesExactVersion(t *testing.T) {
	s := newTestStore(t)
	body := []byte("Synthetic document A")
	hash := sha256.Sum256(body)
	encodedHash := hex.EncodeToString(hash[:])
	node, err := s.CreateFile(t.Context(), s.RootID(), "a.txt", encodedHash, int64(len(body)), "text/plain")
	require.NoError(t, err)
	version, err := s.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	id, err := newUUIDv4()
	require.NoError(t, err)
	occurrence := strings.Repeat("a", 32)
	request := SnapshotSealRequest{SnapshotID: id, Members: []CollectionSnapshotMember{{
		Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID,
		ContentVersionID: version.ID, BlobSHA256: version.BlobHash, Size: version.Size,
		FamilyID: occurrence, FamilyOrder: 1, DocumentKind: "other",
		DisplayName: node.Name, FrozenFieldsJSON: "{}",
		Representations: []CollectionSnapshotRepresentation{{
			OccurrenceID: occurrence, Role: "native", Status: "available",
			TextAuthority: "none", ContentVersionID: version.ID,
			BlobSHA256: version.BlobHash, Size: version.Size, MediaType: "text/plain",
		}},
	}}}
	sealed, err := s.SealCollectionSnapshot(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, 1, sealed.MemberCount)
	require.True(t, canonical.IsSHA256Hex(sealed.MemberHash))
	require.True(t, canonical.IsSHA256Hex(sealed.ManifestSHA256))
	require.True(t, canonical.IsSHA256Hex(sealed.Checksum))
	require.NotEqual(t, sealed.ManifestSHA256, sealed.Checksum)
	read, err := s.CollectionSnapshot(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, sealed, read)
	members, err := s.SnapshotMembers(t.Context(), id, 0, 100)
	require.NoError(t, err)
	require.Equal(t, request.Members, members)

	_, err = s.db.Exec(`UPDATE collection_snapshot_members SET size=99 WHERE snapshot_id=?`, id)
	require.ErrorContains(t, err, "collection snapshot members are immutable")
	again, err := s.SealCollectionSnapshot(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, sealed, again)

	invalidID, err := newUUIDv4()
	require.NoError(t, err)
	request.SnapshotID = invalidID
	request.Members[0].Representations[0].BlobSHA256 = fakeHash("d1")
	_, err = s.SealCollectionSnapshot(t.Context(), request)
	require.ErrorIs(t, err, ErrPackageConflict, "a role cannot claim unknown bytes")
	request.Members[0].Representations[0].BlobSHA256 = version.BlobHash
	request.Members[0].ParentOccurrenceID = occurrence
	_, err = s.SealCollectionSnapshot(t.Context(), request)
	require.ErrorIs(t, err, ErrPackageConflict, "a family cannot contain itself")
	request.Members[0].ParentOccurrenceID = ""

	request.Members[0].Size++
	_, err = s.SealCollectionSnapshot(t.Context(), request)
	require.ErrorIs(t, err, ErrPackageConflict)
}

func TestSnapshotSourceCollectionsFreezeMembership(t *testing.T) {
	s := newTestStore(t)
	body := []byte("Synthetic collection document")
	hash := sha256.Sum256(body)
	node, err := s.CreateFile(t.Context(), s.RootID(), "source.txt", hex.EncodeToString(hash[:]), int64(len(body)), "text/plain")
	require.NoError(t, err)
	first, err := newUUIDv4()
	require.NoError(t, err)
	second, err := newUUIDv4()
	require.NoError(t, err)
	for _, collectionID := range []string{first, second} {
		_, err = s.db.Exec(`INSERT INTO ingests(id,started_at,source_kind,source_desc) VALUES(?,?,?,?)`,
			collectionID, nowRFC3339(), "file", "Synthetic collection")
		require.NoError(t, err)
		provenanceID, idErr := newUUIDv4()
		require.NoError(t, idErr)
		_, err = s.db.Exec(`INSERT INTO provenance(identity,node_id,ingest_id,original_path) VALUES(?,?,?,?)`,
			provenanceID, node.ID, collectionID, "source.txt")
		require.NoError(t, err)
	}
	snapshotID, err := newUUIDv4()
	require.NoError(t, err)
	occurrence := strings.Repeat("b", 32)
	member := CollectionSnapshotMember{
		Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID,
		ContentVersionID: node.CurrentVersionID, BlobSHA256: hex.EncodeToString(hash[:]),
		Size: int64(len(body)), FamilyID: occurrence, FamilyOrder: 1,
		DisplayName: node.Name, FrozenFieldsJSON: "{}", DocumentKind: "other",
		SourceCollectionIDs: []string{first, second},
	}
	request := SnapshotSealRequest{SnapshotID: snapshotID, SourceCollectionIDs: []string{second, first, second}, Members: []CollectionSnapshotMember{member}}
	sealed, err := s.SealCollectionSnapshot(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, sealed.SourceCollectionIDs, 2)
	wantCollections := []string{first, second}
	slices.Sort(wantCollections)
	require.Equal(t, wantCollections, sealed.SourceCollectionIDs)
	duplicateID, err := newUUIDv4()
	require.NoError(t, err)
	duplicate := member
	duplicate.Ordinal = 2
	duplicate.OccurrenceID = strings.Repeat("d", 32)
	duplicate.FamilyID = duplicate.OccurrenceID
	request.SnapshotID = duplicateID
	request.Members = []CollectionSnapshotMember{member, duplicate}
	_, err = s.SealCollectionSnapshot(t.Context(), request)
	require.ErrorIs(t, err, ErrPackageConflict, "overlapping collections must not duplicate one node/version")
	request.Members = []CollectionSnapshotMember{member}

	unknown, err := newUUIDv4()
	require.NoError(t, err)
	newID, err := newUUIDv4()
	require.NoError(t, err)
	request.SnapshotID = newID
	request.SourceCollectionIDs = []string{unknown}
	_, err = s.SealCollectionSnapshot(t.Context(), request)
	require.ErrorIs(t, err, ErrPackageConflict)

	replacement := []byte("Replaced")
	replacementHash := sha256.Sum256(replacement)
	_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, hex.EncodeToString(replacementHash[:]), int64(len(replacement)), "text/plain")
	require.NoError(t, err)
	request.SourceCollectionIDs = []string{first}
	_, err = s.SealCollectionSnapshot(t.Context(), request)
	require.ErrorIs(t, err, ErrPackageConflict)
	read, err := s.CollectionSnapshot(t.Context(), snapshotID)
	require.NoError(t, err)
	require.Equal(t, sealed, read)
}

func TestSnapshotSelectedPagesUseVerifiedFullPDF(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "eight.pdf", fakeHash("a1"), 123, "application/pdf")
	require.NoError(t, err)
	source := document.PageSource{VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: 123}
	pages := []int{1, 2, 3, 4, 5, 6, 7, 8}
	operationID, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.QueuePageJob(t.Context(), operationID, PageJobRequest{
		Source: source, NodeID: node.ID, Revision: node.Revision, Pages: pages,
		DPI: 144, RuntimeFingerprint: fakeHash("b1"),
	})
	require.NoError(t, err)
	claim, err := s.ClaimPageJob(t.Context())
	require.NoError(t, err)
	frames := make([]document.PageFrameV1, 0, 8)
	for _, page := range pages {
		frame, frameErr := document.NewPDFPageFrame(source, page, [4]float64{0, 0, 72, 144}, [4]float64{0, 0, 72, 144}, 0)
		require.NoError(t, frameErr)
		frames = append(frames, frame)
	}
	require.NoError(t, s.PublishPageFrames(t.Context(), claim, frames))
	occurrence := strings.Repeat("c", 32)
	member := CollectionSnapshotMember{
		Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID,
		ContentVersionID: node.CurrentVersionID, BlobSHA256: node.BlobHash, Size: 123,
		FamilyID: occurrence, FamilyOrder: 1, DisplayName: node.Name,
		FrozenFieldsJSON: "{}", DocumentKind: "other",
		SourcePageCount: 8, SelectedPDFSHA256: node.BlobHash,
		SelectedSourcePages: []int{3, 6},
		Representations: []CollectionSnapshotRepresentation{{
			OccurrenceID: occurrence, Role: "pdf", Status: "available", TextAuthority: "none",
			ContentVersionID: node.CurrentVersionID, BlobSHA256: node.BlobHash,
			Size: 123, MediaType: "application/pdf", VerifiedPageCount: 8,
		}},
	}
	seal := func(m CollectionSnapshotMember) (CollectionSnapshot, error) {
		t.Helper()
		id, idErr := newUUIDv4()
		require.NoError(t, idErr)
		return s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{SnapshotID: id, Members: []CollectionSnapshotMember{m}})
	}
	selected, err := seal(member)
	require.NoError(t, err)
	require.Equal(t, 2, selected.PageCount)
	stored, err := s.SnapshotMembers(t.Context(), selected.SnapshotID, 0, 100)
	require.NoError(t, err)
	require.Equal(t, []int{3, 6}, stored[0].SelectedSourcePages)
	require.Equal(t, 8, stored[0].SourcePageCount)

	all := member
	all.SelectedSourcePages = nil
	allSnapshot, err := seal(all)
	require.NoError(t, err)
	require.Equal(t, 8, allSnapshot.PageCount)
	require.NotEqual(t, selected.ManifestSHA256, allSnapshot.ManifestSHA256)
	stored, err = s.SnapshotMembers(t.Context(), allSnapshot.SnapshotID, 0, 100)
	require.NoError(t, err)
	require.Equal(t, pages, stored[0].SelectedSourcePages)

	for name, change := range map[string]func(*CollectionSnapshotMember){
		"explicit empty": func(m *CollectionSnapshotMember) { m.SelectedSourcePages = []int{} },
		"duplicate":      func(m *CollectionSnapshotMember) { m.SelectedSourcePages = []int{3, 3} },
		"zero":           func(m *CollectionSnapshotMember) { m.SelectedSourcePages = []int{0} },
		"past end":       func(m *CollectionSnapshotMember) { m.SelectedSourcePages = []int{9} },
		"wrong count":    func(m *CollectionSnapshotMember) { m.SourcePageCount = 9 },
		"wrong PDF":      func(m *CollectionSnapshotMember) { m.SelectedPDFSHA256 = fakeHash("c1") },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := member
			change(&invalid)
			_, err := seal(invalid)
			require.ErrorIs(t, err, ErrPackageConflict)
		})
	}
}
