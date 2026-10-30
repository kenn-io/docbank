package store

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVaultChangesRecordMutations(t *testing.T) {
	t.Parallel()
	for _, audited := range []bool{false, true} {
		t.Run(strconv.FormatBool(audited), func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			if audited {
				plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "cli", nil)
				require.NoError(t, err)
				_, err = s.EnableInitialAudit(ctx, plan)
				require.NoError(t, err)
			}
			dir, err := s.Mkdir(ctx, s.RootID(), "inbox")
			require.NoError(t, err)
			node, err := s.CreateFile(ctx, dir.ID, "note.txt", fakeHash("a1"), 4, "text/plain")
			require.NoError(t, err)
			_, _, err = s.ReplaceContent(ctx, node.ID, 1, fakeHash("a2"), 8, "text/plain")
			require.NoError(t, err)
			_, _, _, err = s.RevertContent(ctx, node.ID, 2, node.CurrentVersionID)
			require.NoError(t, err)
			_, _, err = s.Move(ctx, node.ID, dir.ID, "renamed.txt", 3)
			require.NoError(t, err)
			_, err = s.BatchMove(ctx, []BatchMoveRequest{{NodeID: node.ID, IfRevision: 4, DestinationPath: "/inbox/final.txt"}})
			require.NoError(t, err)
			tag, err := s.CreateTag(ctx, "receipts")
			require.NoError(t, err)
			_, err = s.AssignTag(ctx, tag.ID, node.ID, 5)
			require.NoError(t, err)
			unchanged, err := s.AssignTag(ctx, tag.ID, node.ID, 6)
			require.NoError(t, err)
			require.False(t, unchanged.Changed)
			_, err = s.UnassignTag(ctx, tag.ID, node.ID, 6)
			require.NoError(t, err)
			operation, err := newUUIDv4()
			require.NoError(t, err)
			_, err = s.BatchTags(ctx, BatchTagRequest{OperationID: operation, TagID: tag.ID, Assign: true, Nodes: []BatchTagTarget{{NodeID: node.ID, Revision: 7}}})
			require.NoError(t, err)
			tag, err = s.RenameTag(ctx, tag.ID, UnconditionalRev, "invoices")
			require.NoError(t, err)
			_, err = s.DeleteTag(ctx, tag.ID, tag.Revision)
			require.NoError(t, err)
			_, _, err = s.Trash(ctx, dir.ID, UnconditionalRev)
			require.NoError(t, err)
			_, _, err = s.Restore(ctx, dir.ID, UnconditionalRev)
			require.NoError(t, err)
			changes, head, err := s.VaultChanges(ctx, "", 500)
			require.NoError(t, err)
			type event struct {
				kind         string
				id, revision int64
			}
			got := make([]event, 0, len(changes))
			for i, change := range changes {
				got = append(got, event{change.Kind, change.NodeID, change.Revision})
				raw, err := base64.RawURLEncoding.DecodeString(change.Cursor)
				require.NoError(t, err)
				require.True(t, strings.HasSuffix(string(raw), fmt.Sprintf(":%d", i+1)), "committed sequences must have no gaps")
			}
			require.Equal(t, []event{
				{"created", dir.ID, 1}, {"created", node.ID, 1},
				{"version_added", node.ID, 2}, {"version_added", node.ID, 3},
				{"moved", node.ID, 4}, {"moved", node.ID, 5},
				{"tagged", node.ID, 6}, {"tagged", node.ID, 7}, {"tagged", node.ID, 8}, {"tagged", node.ID, 9}, {"tagged", node.ID, 10},
				{"trashed", dir.ID, 5}, {"trashed", node.ID, 11}, {"restored", dir.ID, 6}, {"restored", node.ID, 12},
			}, got)
			require.Equal(t, changes[len(changes)-1].Cursor, head)
			page, _, err := s.VaultChanges(ctx, changes[0].Cursor, 2)
			require.NoError(t, err)
			require.Equal(t, changes[1:3], page)
		})
	}
}

func TestVaultChangesCursors(t *testing.T) { //nolint:paralleltest // Pruning coverage changes process-wide retention.
	// Retention is process-wide. This test runs before parallel store tests.
	prior := vaultChangeRetention
	vaultChangeRetention = 2
	t.Cleanup(func() { vaultChangeRetention = prior })
	s := newTestStore(t)
	ctx := t.Context()
	_, empty, err := s.VaultChanges(ctx, "", 500)
	require.NoError(t, err)
	for _, name := range []string{"one", "two", "three"} {
		_, err = s.Mkdir(ctx, s.RootID(), name)
		require.NoError(t, err)
	}
	changes, head, err := s.VaultChanges(ctx, "", 500)
	require.NoError(t, err)
	require.Len(t, changes, 2)
	raw, err := base64.RawURLEncoding.DecodeString(head)
	require.NoError(t, err)
	epoch, _, _ := strings.Cut(string(raw), ":")
	for _, cursor := range []string{empty, changeCursor("11111111-1111-4111-8111-111111111111", 0), changeCursor(epoch, 4)} {
		_, current, err := s.VaultChanges(ctx, cursor, 500)
		require.ErrorIs(t, err, ErrChangeCursorExpired)
		require.Equal(t, head, current)
	}
	page, _, err := s.VaultChanges(ctx, head, 500)
	require.NoError(t, err)
	require.Empty(t, page)
}
