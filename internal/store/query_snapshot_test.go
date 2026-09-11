package store

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

func snapshotTestQuery(t *testing.T, raw string) query.Query {
	t.Helper()
	value, err := query.Parse([]byte(raw))
	require.NoError(t, err)
	return value
}

func independentMemberHash(members []SnapshotMember) string {
	members = slices.Clone(members)
	slices.SortFunc(members, func(left, right SnapshotMember) int {
		if result := cmp.Compare(left.NodeID, right.NodeID); result != 0 {
			return result
		}
		return strings.Compare(left.ContentVersionID, right.ContentVersionID)
	})
	var input strings.Builder
	for _, member := range members {
		_, _ = fmt.Fprintf(&input, "%d:%s\n", member.NodeID, member.ContentVersionID)
	}
	digest := sha256.Sum256([]byte(input.String()))
	return hex.EncodeToString(digest[:])
}

// Reversing the presentation input must not change the identity receipt, and
// lexically sorting decimal node IDs would put 10 before 2.
func TestQuerySnapshotMemberHashUsesNumericIdentityOrder(t *testing.T) {
	members := []SnapshotMember{
		{NodeID: 10, ContentVersionID: "v10"},
		{NodeID: 2, ContentVersionID: "v2"},
	}
	want := sha256.Sum256([]byte("2:v2\n10:v10\n"))
	assert.Equal(t, hex.EncodeToString(want[:]), snapshotMemberHash(members))
	assert.Equal(t, hex.EncodeToString(want[:]), snapshotMemberHash([]SnapshotMember{members[1], members[0]}))
}

// Omitting member identity, source size, or frozen metadata from the selected
// read view makes this fail even if a live search still returns both files.
func TestQuerySnapshotMaterializesFrozenRowsAndReceipts(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	run := createCollectionRun(t, s, "zeta.TXT", "snapshot-zeta")
	zeta, err := s.NodeByPath(ctx, "/zeta.TXT")
	require.NoError(t, err)
	alpha, _, err := s.IngestFile(ctx, run, s.RootID(), "alpha.pdf", fakeHash("snapshot-alpha"),
		17, "application/pdf", "/synthetic/alpha.pdf", "")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "review")
	require.NoError(t, err)
	assigned, err := s.AssignTag(ctx, tag.ID, alpha.ID, alpha.Revision)
	require.NoError(t, err)
	alpha = assigned.Node

	value := snapshotTestQuery(t, `{}`)
	projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, projection.Rows, 2)
	assert.Equal(t, []string{"alpha.pdf", "zeta.TXT"}, []string{
		projection.Rows[0].Name, projection.Rows[1].Name,
	})
	assert.Equal(t, "/alpha.pdf", projection.Rows[0].Path)
	assert.Equal(t, "document", projection.Rows[0].MediaFamily)
	assert.Equal(t, "pdf", query.FilenameExtension(projection.Rows[0].Name))
	assert.Equal(t, []SnapshotTag{{ID: assigned.Tag.ID, Name: assigned.Tag.Name, Revision: assigned.Tag.Revision}}, projection.Rows[0].Tags)
	assert.Equal(t, []string{run.ID()}, projection.Rows[0].CollectionIDs)
	assert.Equal(t, run.ID(), projection.Rows[0].DisplayCollectionID)
	assert.Nil(t, projection.Rows[0].DisplayCollectionLabel)
	assert.Equal(t, int64(2), projection.Total)
	assert.Equal(t, alpha.Size+zeta.Size, projection.TotalBytes)
	assert.Equal(t, "sha256:5e60ffa8c5c733830f8dddba716f9bfce8a17121b5f8eb4de97cf23cf2fbe460", projection.QueryFingerprint)
	assert.Equal(t, SnapshotGeneration{Kind: "native"}, projection.Generation)
	assert.Equal(t, 100, projection.PageSize)
	assert.Positive(t, projection.SerializedBytes)
	assert.True(t, strings.HasPrefix(projection.SnapshotFingerprint, "sha256:"))

	members := []SnapshotMember{projection.Rows[0].SnapshotMember, projection.Rows[1].SnapshotMember}
	assert.Equal(t, independentMemberHash(members), projection.MemberHash)

	originalRows := projection.Rows
	originalHash := projection.MemberHash
	originalSnapshotFingerprint := projection.SnapshotFingerprint
	lateTag, err := s.CreateTag(ctx, "late")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, lateTag.ID, zeta.ID, zeta.Revision)
	require.NoError(t, err)
	assert.Equal(t, originalRows, projection.Rows)
	assert.Equal(t, originalHash, projection.MemberHash)
	assert.Equal(t, originalSnapshotFingerprint, projection.SnapshotFingerprint)
	refreshed, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	assert.Equal(t, originalHash, refreshed.MemberHash, "mutable metadata is not content membership identity")
	assert.NotEqual(t, originalSnapshotFingerprint, refreshed.SnapshotFingerprint,
		"a new frozen projection records the changed tag and node revision")
}

func TestQuerySnapshotRejectsBoundsDuringMaterialization(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		_, err := s.CreateFile(ctx, s.RootID(), name, fakeHash("snapshot-"+name), 1, "text/plain")
		require.NoError(t, err)
	}
	value := snapshotTestQuery(t, `{}`)

	_, err := s.materializeQuerySnapshot(ctx, SnapshotRequest{Query: value}, snapshotMaterializeOptions{MaxRows: 2})
	require.ErrorIs(t, err, ErrQuerySnapshotTooLarge)
	_, err = s.materializeQuerySnapshot(ctx, SnapshotRequest{Query: value}, snapshotMaterializeOptions{MaxSerializedBytes: 1})
	require.ErrorIs(t, err, ErrQuerySnapshotTooLarge)

	rowCharges := 0
	_, err = s.materializeQuerySnapshot(ctx, SnapshotRequest{Query: value}, snapshotMaterializeOptions{
		Charge: func(rows, bytes int64) error {
			if rows > 0 {
				rowCharges++
			}
			if rowCharges == 2 {
				return assert.AnError
			}
			return nil
		},
	})
	require.ErrorIs(t, err, assert.AnError)
	assert.Equal(t, 2, rowCharges, "admission must interrupt row retention instead of checking after full allocation")
}

func TestQuerySnapshotSortsTypedPrimaryAndAscendingIdentityTies(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "z.txt", fakeHash("sort-first"), 10, "TEXT/PLAIN; Charset=UTF-8")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "a.pdf", fakeHash("sort-second"), 2, "application/pdf")
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE nodes SET modified_at=? WHERE id=?`, "2026-01-02T00:00:00Z", first.ID)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE nodes SET modified_at=? WHERE id=?`, "2026-01-01T00:00:00Z", second.ID)
	require.NoError(t, err)

	tests := []struct {
		field, direction string
		want             []int64
		keys             []string
	}{
		{"name", "desc", []int64{first.ID, second.ID}, []string{"z.txt", "a.pdf"}},
		{"size", "asc", []int64{second.ID, first.ID}, []string{"2", "10"}},
		{"modified_at", "asc", []int64{second.ID, first.ID}, []string{"2026-01-01T00:00:00.000000000Z", "2026-01-02T00:00:00.000000000Z"}},
		{"media_type", "asc", []int64{second.ID, first.ID}, []string{"application/pdf", "text/plain"}},
	}
	for _, testCase := range tests {
		t.Run(testCase.field, func(t *testing.T) {
			value := snapshotTestQuery(t, fmt.Sprintf(`{"sort":{"field":%q,"direction":%q}}`, testCase.field, testCase.direction))
			projection, err := s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: value})
			require.NoError(t, err)
			assert.Equal(t, testCase.want, []int64{projection.Rows[0].NodeID, projection.Rows[1].NodeID})
			assert.Equal(t, []string{projection.Rows[0].SortKey, projection.Rows[1].SortKey}, testCase.keys)
		})
	}

	second, _, err = s.ReplaceContent(ctx, second.ID, second.Revision, fakeHash("sort-second-tie"), 10, second.MimeType)
	require.NoError(t, err)
	value := snapshotTestQuery(t, `{"sort":{"field":"size","direction":"desc"}}`)
	projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	assert.Equal(t, []int64{first.ID, second.ID}, []int64{projection.Rows[0].NodeID, projection.Rows[1].NodeID},
		"identity ties stay ascending even when the primary direction is descending")
}
