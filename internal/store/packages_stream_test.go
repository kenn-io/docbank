package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

type membershipCountingQuerier struct {
	*sql.Tx

	queries int
}

func (q *membershipCountingQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.queries++
	return q.Tx.QueryContext(ctx, query, args...)
}

func TestSnapshotSourceBatchChecksOverlappingMembershipInOneQuery(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	const body = "Synthetic overlapping collection source"
	hash := sha256.Sum256([]byte(body))
	ids := make([]string, 2)
	for i := range ids {
		var err error
		ids[i], err = newUUIDv4()
		require.NoError(t, err)
		_, err = s.db.Exec(`INSERT INTO ingests(id,started_at,source_kind,source_desc) VALUES(?,?,?,?)`, ids[i], nowRFC3339(), "file", "Synthetic collection")
		require.NoError(t, err)
	}
	members := make([]CollectionSnapshotMember, 3)
	for i := range members {
		node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("source-%d.txt", i), hex.EncodeToString(hash[:]), int64(len(body)), "text/plain")
		require.NoError(t, err)
		members[i] = CollectionSnapshotMember{NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
		for _, id := range ids {
			if i == 2 && id == ids[0] {
				continue
			}
			provenanceID, err := provenanceIdentity(metadataProvenance{Type: metadataProvenanceType, NodeID: node.ID, IngestID: id, OriginalPath: node.Name})
			require.NoError(t, err)
			_, err = s.db.Exec(`INSERT INTO provenance(identity,node_id,ingest_id,original_path) VALUES(?,?,?,?)`, provenanceID, node.ID, id, node.Name)
			require.NoError(t, err)
		}
		members[i].SourceCollectionIDs = slices.Clone(ids)
		if i == 2 {
			members[i].SourceCollectionIDs = []string{ids[1]}
		}
		slices.Sort(members[i].SourceCollectionIDs)
	}
	tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	q := &membershipCountingQuerier{Tx: tx}
	matched := make(map[string]bool)
	require.NoError(t, matchSnapshotSourcesBatch(t.Context(), q, ids, matched, members))
	require.Equal(t, 1, q.queries, "one membership query covers the batch and both sources")
	require.True(t, matched[ids[0]])
	require.True(t, matched[ids[1]])

	members[0].ContentVersionID = "00000000-0000-4000-8000-000000000001"
	require.ErrorIs(t, matchSnapshotSourcesBatch(t.Context(), q, ids, matched, members), ErrPackageConflict)
}

func streamSnapshotMembers(t *testing.T, members ...CollectionSnapshotMember) *bytes.Reader {
	t.Helper()
	var lines bytes.Buffer
	for _, member := range members {
		encoded, err := canonical.Marshal(member)
		require.NoError(t, err)
		lines.Write(encoded)
		lines.WriteByte('\n')
	}
	return bytes.NewReader(lines.Bytes())
}

func TestSealCollectionSnapshotStreamAcceptsLargeManifestAndMatchesCanonicalDigest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	content := "Synthetic stream source"
	hash := sha256.Sum256([]byte(content))
	node, err := s.CreateFile(t.Context(), s.RootID(), "stream.txt", hex.EncodeToString(hash[:]), int64(len(content)), "text/plain")
	require.NoError(t, err)
	version, err := s.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	id, err := newUUIDv4()
	require.NoError(t, err)
	occurrence := strings.Repeat("a", 32)
	member := CollectionSnapshotMember{
		Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID,
		ContentVersionID: version.ID, BlobSHA256: version.BlobHash, Size: version.Size,
		FamilyID: occurrence, FamilyOrder: 1, DocumentKind: "other",
		DisplayName: node.Name, FrozenFieldsJSON: fmt.Sprintf(`{"note":"%s"}`, strings.Repeat("x", 65<<10)),
		Representations: []CollectionSnapshotRepresentation{{OccurrenceID: occurrence,
			Role: "native", Status: "available", TextAuthority: "none",
			ContentVersionID: version.ID, BlobSHA256: version.BlobHash, Size: version.Size, MediaType: "text/plain"}},
	}
	_, err = s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{SnapshotID: id, Members: []CollectionSnapshotMember{member}})
	require.ErrorIs(t, err, ErrPackageConflict, "inline requests retain the 64 KiB limit")
	sealed, err := s.SealCollectionSnapshotStream(t.Context(), SnapshotSealHeader{SnapshotID: id}, streamSnapshotMembers(t, member))
	require.NoError(t, err)
	manifest, err := canonical.Marshal(struct {
		SourceCollectionIDs []string                   `json:"source_collection_ids"`
		Members             []CollectionSnapshotMember `json:"members"`
	}{[]string{}, []CollectionSnapshotMember{member}})
	require.NoError(t, err)
	want := sha256.Sum256(manifest)
	require.Equal(t, hex.EncodeToString(want[:]), sealed.ManifestSHA256)
	stored, err := s.SnapshotMembers(t.Context(), id, 0, 2)
	require.NoError(t, err)
	require.Equal(t, []CollectionSnapshotMember{member}, stored)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	var again bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &again))
	require.Equal(t, exported.Bytes(), again.Bytes())
}

func TestSealCollectionSnapshotStreamRollsBackAndReplaysExactly(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	content := "Synthetic stream source"
	hash := sha256.Sum256([]byte(content))
	node, err := s.CreateFile(t.Context(), s.RootID(), "stream.txt", hex.EncodeToString(hash[:]), int64(len(content)), "text/plain")
	require.NoError(t, err)
	version, err := s.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	id, err := newUUIDv4()
	require.NoError(t, err)
	occurrence := strings.Repeat("b", 32)
	member := CollectionSnapshotMember{Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID,
		ContentVersionID: version.ID, BlobSHA256: version.BlobHash, Size: version.Size,
		FamilyID: occurrence, FamilyOrder: 1, DocumentKind: "other", DisplayName: node.Name, FrozenFieldsJSON: "{}"}
	bad := member
	bad.Ordinal = 2
	bad.OccurrenceID = strings.Repeat("c", 32)
	bad.NodeID++
	bad.FamilyID = bad.OccurrenceID
	_, err = s.SealCollectionSnapshotStream(t.Context(), SnapshotSealHeader{SnapshotID: id}, streamSnapshotMembers(t, member, bad))
	require.ErrorIs(t, err, ErrPackageConflict)
	_, err = s.CollectionSnapshot(t.Context(), id)
	require.ErrorIs(t, err, ErrNotFound)
	sealed, err := s.SealCollectionSnapshotStream(t.Context(), SnapshotSealHeader{SnapshotID: id}, streamSnapshotMembers(t, member))
	require.NoError(t, err)
	inlineID, err := newUUIDv4()
	require.NoError(t, err)
	inline, err := s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{SnapshotID: inlineID, Members: []CollectionSnapshotMember{member}})
	require.NoError(t, err)
	require.Equal(t, inline.ManifestSHA256, sealed.ManifestSHA256)
	require.Equal(t, inline.MemberHash, sealed.MemberHash)
	replay, err := s.SealCollectionSnapshotStream(t.Context(), SnapshotSealHeader{SnapshotID: id}, streamSnapshotMembers(t, member))
	require.NoError(t, err)
	require.Equal(t, sealed, replay)
	member.DisplayName = "different.txt"
	_, err = s.SealCollectionSnapshotStream(t.Context(), SnapshotSealHeader{SnapshotID: id}, streamSnapshotMembers(t, member))
	require.ErrorIs(t, err, ErrPackageConflict)
}

func TestSealCollectionSnapshotStreamRejectsOversizedRowWithoutSealing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	id, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.SealCollectionSnapshotStream(t.Context(), SnapshotSealHeader{SnapshotID: id}, strings.NewReader(strings.Repeat("x", (1<<20)+1)+"\n"))
	require.ErrorIs(t, err, ErrPackageConflict)
	_, err = s.CollectionSnapshot(t.Context(), id)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSealCollectionSnapshotStreamValidatesFamilyAcrossBatches(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	const count = 1001
	members := make([]CollectionSnapshotMember, 0, count)
	sourceIDs := make([]string, 2)
	for i := range sourceIDs {
		var err error
		sourceIDs[i], err = newUUIDv4()
		require.NoError(t, err)
		_, err = s.db.Exec(`INSERT INTO ingests(id,started_at,source_kind,source_desc) VALUES(?,?,?,?)`, sourceIDs[i], nowRFC3339(), "file", "Synthetic batch collection")
		require.NoError(t, err)
	}
	content := "Synthetic batch source"
	hash := sha256.Sum256([]byte(content))
	root := fmt.Sprintf("%032x", 1)
	type sourceNode struct {
		id      int64
		name    string
		ordinal int
	}
	var sourceNodes []sourceNode
	for ordinal := 1; ordinal <= count; ordinal++ {
		node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("batch-%04d.txt", ordinal), hex.EncodeToString(hash[:]), int64(len(content)), "text/plain")
		require.NoError(t, err)
		version, err := s.ContentVersionByID(t.Context(), node.CurrentVersionID)
		require.NoError(t, err)
		member := CollectionSnapshotMember{Ordinal: ordinal, OccurrenceID: fmt.Sprintf("%032x", ordinal),
			NodeID: node.ID, ContentVersionID: version.ID, BlobSHA256: version.BlobHash,
			Size: version.Size, FamilyID: root, FamilyOrder: ordinal,
			DocumentKind: "other", DisplayName: node.Name, FrozenFieldsJSON: "{}"}
		if ordinal > 1 {
			member.ParentOccurrenceID = root
		}
		members = append(members, member)
		sourceNodes = append(sourceNodes, sourceNode{node.ID, node.Name, ordinal})
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	for _, node := range sourceNodes {
		for sourceIndex, sourceID := range sourceIDs {
			if sourceIndex == 1 && node.ordinal != count {
				continue
			}
			provenanceID, err := provenanceIdentity(metadataProvenance{Type: metadataProvenanceType, NodeID: node.id, IngestID: sourceID, OriginalPath: node.name})
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO provenance(identity,node_id,ingest_id,original_path) VALUES(?,?,?,?)`, provenanceID, node.id, sourceID, node.name)
			require.NoError(t, err)
		}
	}
	require.NoError(t, tx.Commit())
	for index := range members {
		members[index].SourceCollectionIDs = []string{sourceIDs[0]}
		if index == len(members)-1 {
			members[index].SourceCollectionIDs = slices.Clone(sourceIDs)
			slices.Sort(members[index].SourceCollectionIDs)
		}
	}
	id, err := newUUIDv4()
	require.NoError(t, err)
	sealed, err := s.SealCollectionSnapshotStream(t.Context(), SnapshotSealHeader{SnapshotID: id, SourceCollectionIDs: sourceIDs}, streamSnapshotMembers(t, members...))
	require.NoError(t, err)
	require.Equal(t, count, sealed.MemberCount)
	require.NoError(t, validateSnapshotMetadataRows(t.Context(), s.db, sealed))
	require.NoError(t, s.ValidateMetadata(t.Context()))
	wrongMemberHash := sealed
	wrongMemberHash.MemberHash = fakeHash("ab")
	require.ErrorIs(t, validateSnapshotMetadataRows(t.Context(), s.db, wrongMemberHash), ErrPackageConflict)
	wrongManifest := sealed
	wrongManifest.ManifestSHA256 = fakeHash("cd")
	require.ErrorIs(t, validateSnapshotMetadataRows(t.Context(), s.db, wrongManifest), ErrPackageConflict)
	page, err := s.SnapshotMembers(t.Context(), id, 1000, 2)
	require.NoError(t, err)
	require.Equal(t, []CollectionSnapshotMember{members[1000]}, page)

	badID, err := newUUIDv4()
	require.NoError(t, err)
	members[1000].ParentOccurrenceID = members[1000].OccurrenceID
	_, err = s.SealCollectionSnapshotStream(t.Context(), SnapshotSealHeader{SnapshotID: badID, SourceCollectionIDs: sourceIDs}, streamSnapshotMembers(t, members...))
	require.ErrorIs(t, err, ErrPackageConflict)
	_, err = s.CollectionSnapshot(t.Context(), badID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSnapshotFamilyGraphAllowsGroupsWithValidRepresentatives(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		parents  map[string]string
		families map[string]string
		valid    bool
	}{
		{"grouped_roots", map[string]string{"a": "", "b": "", "c": "b"},
			map[string]string{"a": "a", "b": "a", "c": "a"}, true},
		{"missing_representative", map[string]string{"a": ""},
			map[string]string{"a": "b"}, false},
		{"representative_has_parent", map[string]string{"a": "", "b": "a"},
			map[string]string{"a": "b", "b": "b"}, false},
		{"representative_in_other_family", map[string]string{"a": "", "b": "", "c": ""},
			map[string]string{"a": "b", "b": "c", "c": "c"}, false},
		{"parent_in_other_family", map[string]string{"a": "", "b": "", "c": "b"},
			map[string]string{"a": "a", "b": "b", "c": "a"}, false},
		{"parent_cycle", map[string]string{"a": "", "b": "c", "c": "b"},
			map[string]string{"a": "a", "b": "a", "c": "a"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.valid, validSnapshotFamilyGraph(test.parents, test.families))
		})
	}
}
