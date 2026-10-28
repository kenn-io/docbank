package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedWatchRebind(t *testing.T, s *Store, ref, name string) Node {
	t.Helper()
	run, err := s.BeginIngest(t.Context(), "watch", "notes")
	require.NoError(t, err)
	node, err := s.IngestFileExact(t.Context(), run, s.RootID(), name,
		fakeHash("a1"), 5, "text/plain", ref, "")
	require.NoError(t, err)
	return node
}

func TestRebindWatchedSourcePreservesHistoryAndAdoptsAtCurrentRef(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := seedWatchRebind(t, s, "draft.md", "draft.md")
	edited, _, err := s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("b2"), 6, "text/plain")
	require.NoError(t, err)
	rebound, err := s.RebindWatchedSource(ctx, "notes", "draft.md", "notes/final.md", node.ID)
	require.NoError(t, err)
	assert.Equal(t, edited, rebound)
	retry, err := s.RebindWatchedSource(ctx, "notes", "draft.md", "notes/final.md", node.ID)
	require.NoError(t, err)
	assert.Equal(t, edited, retry)
	cursors, err := s.PushCursors(ctx, "notes", "", 10)
	require.NoError(t, err)
	require.Len(t, cursors, 1)
	assert.Equal(t, "notes/final.md", cursors[0].Ref)
	assert.Equal(t, node.ID, cursors[0].NodeID)
	assert.Equal(t, fakeHash("a1"), cursors[0].Hash)
	var original string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT original_path FROM provenance WHERE node_id=?`, node.ID).Scan(&original))
	assert.Equal(t, "draft.md", original)
	var snapshot bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &snapshot))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(snapshot.Bytes())))
	require.NoError(t, restored.ValidateMetadata(ctx))
	restoredCursors, err := restored.PushCursors(ctx, "notes", "", 10)
	require.NoError(t, err)
	require.Equal(t, cursors, restoredCursors)
	after := requireNodeByID(t, restored, node.ID)
	assert.Equal(t, edited.CurrentVersionID, after.CurrentVersionID)
	_, total, err := restored.ContentVersions(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	_, outcome, err := restored.AcceptPush(ctx, PushSource{Name: "notes", Ref: "notes/final.md"},
		PushContent{RunID: testRunID(t), ParentPath: "/", Name: "final.md", Hash: fakeHash("a1"), Size: 5, MIMEType: "text/plain"})
	require.NoError(t, err)
	assert.Equal(t, "skipped", outcome)
	adopted := requirePushCursor(t, restored, PushSource{Name: "notes", Ref: "notes/final.md"})
	assert.Equal(t, node.ID, adopted.NodeID)
}

func TestRebindWatchedSourceRejectsOwnersAndDestinationConflicts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *Store, Node)
		retry bool
	}{
		{"watch destination", func(t *testing.T, s *Store, _ Node) {
			t.Helper()
			seedWatchRebind(t, s, "notes/final.md", "other.md")
		}, false},
		{"push owns node", func(t *testing.T, s *Store, node Node) {
			t.Helper()
			_, _, err := s.AcceptPush(t.Context(), PushSource{Name: "notes", Ref: "draft.md"},
				PushContent{RunID: testRunID(t), ParentPath: "/", Name: "draft.md", Hash: fakeHash("b2"), Size: 6, MIMEType: "text/plain"})
			require.NoError(t, err)
		}, false},
		{"push destination key", func(t *testing.T, s *Store, _ Node) {
			t.Helper()
			_, _, err := s.AcceptPush(t.Context(), PushSource{Name: "notes", Ref: "notes/final.md"},
				PushContent{RunID: testRunID(t), ParentPath: "/", Name: "pushed.md", Hash: fakeHash("c3"), Size: 7, MIMEType: "text/plain"})
			require.NoError(t, err)
		}, false},
		{"push destination key on retry", func(t *testing.T, s *Store, node Node) {
			t.Helper()
			_, err := s.RebindWatchedSource(t.Context(), "notes", "draft.md", "notes/final.md", node.ID)
			require.NoError(t, err)
			_, _, err = s.AcceptPush(t.Context(), PushSource{Name: "notes", Ref: "other.md"},
				PushContent{RunID: testRunID(t), ParentPath: "/", Name: "pushed.md", Hash: fakeHash("c3"), Size: 7, MIMEType: "text/plain"})
			require.NoError(t, err)
			_, err = s.db.ExecContext(t.Context(), `UPDATE push_sources SET source_ref='notes/final.md' WHERE push_name='notes' AND source_ref='other.md'`)
			require.NoError(t, err)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			node := seedWatchRebind(t, s, "draft.md", "draft.md")
			tc.setup(t, s, node)
			_, err := s.RebindWatchedSource(t.Context(), "notes", "draft.md", "notes/final.md", node.ID)
			require.Error(t, err)
			cursors, err := s.PushCursors(t.Context(), "notes", "", 10)
			require.NoError(t, err)
			if !tc.retry {
				assert.Equal(t, "draft.md", cursors[0].Ref)
			}
			if tc.name == "push destination key" || tc.name == "push destination key on retry" {
				var pushedID int64
				for _, cursor := range cursors {
					if cursor.Ref == "notes/final.md" {
						pushedID = cursor.NodeID
					}
				}
				require.NotZero(t, pushedID)
				assert.NotEqual(t, node.ID, pushedID)
				selected, outcome, err := s.AcceptPush(t.Context(), PushSource{Name: "notes", Ref: "notes/final.md"},
					PushContent{RunID: testRunID(t), ParentPath: "/", Name: "pushed.md",
						Hash: fakeHash("c3"), Size: 7, MIMEType: "text/plain"})
				require.NoError(t, err)
				assert.Equal(t, "skipped", outcome)
				assert.Equal(t, pushedID, selected.ID)
			}
			assert.Equal(t, node.ID, requireNodeByID(t, s, node.ID).ID)
		})
	}
}

func TestReboundWatchPathReuseAndBackupRemainValid(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	seedInitialAuditAuthority(t, s, s.RootID())
	original := seedWatchRebind(t, s, "draft.md", "draft.md")
	manual, _, err := s.ReplaceContent(ctx, original.ID, original.Revision, fakeHash("b2"), 6, "text/plain")
	require.NoError(t, err)
	_, err = s.RebindWatchedSource(ctx, "notes", "draft.md", "notes/final.md", original.ID)
	require.NoError(t, err)
	parent, err := s.MkdirAll(ctx, "/organized")
	require.NoError(t, err)
	_, _, err = s.Move(ctx, original.ID, parent.ID, "moved.md", UnconditionalRev)
	require.NoError(t, err)
	reused := seedWatchRebind(t, s, "draft.md", "draft.md")
	assert.NotEqual(t, original.ID, reused.ID)
	require.NoError(t, s.ValidateMetadata(ctx))
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	snapshot, err := s.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, snapshot.Close()) }()
	var backup bytes.Buffer
	require.NoError(t, snapshot.ExportBackup(ctx, &backup))
	for _, data := range [][]byte{exported.Bytes(), backup.Bytes()} {
		restored := newTestStore(t)
		require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(data)))
		require.NoError(t, restored.ValidateMetadata(ctx))
		cursors, err := restored.PushCursors(ctx, "notes", "", 10)
		require.NoError(t, err)
		require.Len(t, cursors, 2)
		assert.Equal(t, "draft.md", cursors[0].Ref)
		assert.Equal(t, reused.ID, cursors[0].NodeID)
		assert.Equal(t, "notes/final.md", cursors[1].Ref)
		assert.Equal(t, original.ID, cursors[1].NodeID)
		after := requireNodeByID(t, restored, original.ID)
		assert.Equal(t, manual.CurrentVersionID, after.CurrentVersionID)
		_, total, err := restored.ContentVersions(ctx, original.ID, 10, 0)
		require.NoError(t, err)
		assert.Equal(t, 2, total)
	}
}

func TestWatchProvenanceOneOwnerCannotHaveConflictingOriginalPaths(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := seedWatchRebind(t, s, "draft.md", "draft.md")
	run, err := s.BeginIngest(ctx, "watch", "notes")
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := ensureIngestRunTx(ctx, tx, run); err != nil {
			return err
		}
		fact := metadataProvenance{Type: metadataProvenanceType, NodeID: node.ID,
			IngestID: run.ID(), OriginalPath: "different.md"}
		fact.Identity, err = provenanceIdentity(fact)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO provenance(identity,node_id,ingest_id,original_path) VALUES(?,?,?,?)`,
			fact.Identity, fact.NodeID, fact.IngestID, fact.OriginalPath)
		return err
	}))
	err = validateWatchSourceRelations(ctx, s.db)
	require.ErrorContains(t, err, "conflicting original paths")
}

func TestRenamedWatchMetadataRejectsForgedOwner(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := seedWatchRebind(t, s, "draft.md", "draft.md")
	_, err := s.RebindWatchedSource(ctx, "notes", "draft.md", "final.md", node.ID)
	require.NoError(t, err)
	var data bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &data))
	var forged bytes.Buffer
	for line := range bytes.SplitSeq(bytes.TrimSpace(data.Bytes()), []byte{'\n'}) {
		var record metadataWatchSource
		if json.Unmarshal(line, &record) == nil && record.Type == metadataWatchSourceType {
			record.WatchName = "forged"
			line, err = json.Marshal(record)
			require.NoError(t, err)
		}
		forged.Write(line)
		forged.WriteByte('\n')
	}
	target := newTestStore(t)
	require.Error(t, target.ImportMetadata(ctx, bytes.NewReader(forged.Bytes())))
}

func TestRebindWatchedSourceRejectsInvalidRefsAndWrongNode(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := seedWatchRebind(t, s, "draft.md", "draft.md")
	for _, tc := range []struct {
		old, new string
		nodeID   int64
	}{
		{"draft.md", "../outside.md", node.ID},
		{"./draft.md", "final.md", node.ID},
		{"draft.md", "final.md", node.ID + 1},
	} {
		_, err := s.RebindWatchedSource(t.Context(), "notes", tc.old, tc.new, tc.nodeID)
		require.Error(t, err)
	}
	cursors, err := s.PushCursors(t.Context(), "notes", "", 10)
	require.NoError(t, err)
	require.Len(t, cursors, 1)
	assert.Equal(t, "draft.md", cursors[0].Ref)
}

func TestRebindWatchedSourceDoesNotRequireRevisionAdvance(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := seedWatchRebind(t, s, "draft.md", "draft.md")
	_, err := s.db.ExecContext(t.Context(), `UPDATE nodes SET revision=9223372036854775807 WHERE id=?`, node.ID)
	require.NoError(t, err)
	rebound, err := s.RebindWatchedSource(t.Context(), "notes", "draft.md", "final.md", node.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(9223372036854775807), rebound.Revision)
}
