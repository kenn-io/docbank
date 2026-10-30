package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise publication through the runtime role, including audit replay after
// a repeated observation and replacement; syntax-only tests miss these effects.
func TestIngestOperationsPostgres(t *testing.T) {
	t.Parallel()
	for _, audited := range []bool{false, true} {
		name := "ordinary"
		if audited {
			name = "audited"
		}
		t.Run(name, func(t *testing.T) {
			s := newPostgresQueryStore(t)
			ctx := t.Context()
			if audited {
				plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "cli", nil)
				require.NoError(t, err)
				_, err = s.EnableInitialAudit(ctx, plan)
				require.NoError(t, err)
			}
			run, err := s.BeginIngest(ctx, "cli", "Synthetic import")
			require.NoError(t, err)
			node, added, err := s.IngestFileWithMembership(ctx, run, s.RootID(), "note.txt", fakeHash("a1"), 4, "text/plain", "/source/note.txt", "")
			require.NoError(t, err)
			require.True(t, added)
			again, added, err := s.IngestFileWithMembership(ctx, run, s.RootID(), "note.txt", fakeHash("a1"), 4, "text/plain", "/source/note.txt", "")
			require.NoError(t, err)
			require.False(t, added)
			require.Equal(t, node, again)
			second, err := s.BeginIngest(ctx, "cli", "Second import")
			require.NoError(t, err)
			observed, added, err := s.IngestFileWithMembership(ctx, second, s.RootID(), "note.txt", fakeHash("a1"), 4, "text/plain", "/other/note.txt", "")
			require.NoError(t, err)
			require.False(t, added)
			require.Equal(t, node.ID, observed.ID)
			require.Equal(t, node.Revision+1, observed.Revision)
			require.Equal(t, node.CurrentVersionID, observed.CurrentVersionID)
			third, err := s.BeginIngest(ctx, "cli", "Replacement")
			require.NoError(t, err)
			receipt, changed, err := s.ReplaceContentForIngest(ctx, third, node.ID, observed.Revision, fakeHash("a2"), 8, "text/plain", "/source/note.txt", "")
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, node.ID, receipt.Node.ID)
			require.NotEqual(t, node.CurrentVersionID, receipt.Node.CurrentVersionID)
			page, err := s.NodeProvenance(ctx, node.ID, 10, 0)
			require.NoError(t, err)
			require.Equal(t, 3, page.Total)
			require.Len(t, page.Items, 3)
			collections, total, err := s.Collections(ctx, 10, 0)
			require.NoError(t, err)
			require.Equal(t, 3, total)
			require.Len(t, collections, 3)
			members, err := s.CollectionMembers(ctx, run.ID(), 10, 0)
			require.NoError(t, err)
			require.Equal(t, 1, members.Total)
			require.Len(t, members.Items, 1)
			require.Equal(t, receipt.Node, members.Items[0].Node)
			require.Equal(t, int64(8), members.Collection.TotalBytes)
			bindings, err := s.ProvenanceVersionBindings(ctx, node.CurrentVersionID)
			require.NoError(t, err)
			require.Len(t, bindings, 2)
			bindings, err = s.ProvenanceVersionBindings(ctx, receipt.Node.CurrentVersionID)
			require.NoError(t, err)
			require.Len(t, bindings, 1)
			suffixed, added, err := s.IngestFile(ctx, run, s.RootID(), "note.txt", fakeHash("a3"), 12, "text/plain", "/source/note.txt", "")
			require.NoError(t, err)
			require.True(t, added)
			require.Equal(t, "note (2).txt", suffixed.Name)
			embedded, err := s.BeginCallerSuppliedIngest(ctx, "agent", "Source evidence")
			require.NoError(t, err)
			created, err := s.IngestFileExact(ctx, embedded, s.RootID(), "evidence.txt", fakeHash("a4"), 4, "text/plain", "opaque-reference", "")
			require.NoError(t, err)
			confirmed, err := s.ConfirmIngestedContentWithReceipt(ctx, created.ID, created.Revision, created.BlobHash, created.Size, created.MimeType, "agent", "Source evidence", "opaque-reference", "")
			require.NoError(t, err)
			require.Equal(t, created, confirmed.Node)
			facts, err := s.ProvenanceVersionBindings(ctx, created.CurrentVersionID)
			require.NoError(t, err)
			require.Len(t, facts, 1)
			corrected, err := s.AppendNodeProvenance(ctx, ProvenanceAppendInput{NodeID: created.ID, IfRevision: created.Revision, SourceKind: "agent", SourceDescription: "Correction", OriginalPath: "corrected-reference", Supersedes: &facts[0].ProvenanceIdentity})
			require.NoError(t, err)
			require.Equal(t, created.Revision+1, corrected.Node.Revision)
			require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				high, err := s.auditNodeSequence(ctx, tx)
				if err != nil {
					return err
				}
				return s.validateMetadataState(ctx, tx, high)
			}))
		})
	}
}

func TestPlannedIngestPostgres(t *testing.T) {
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	run, err := s.BeginIngestWithLabel(ctx, "cli", "Synthetic folder", new("Inbox"))
	require.NoError(t, err)
	plan, err := s.PrepareIngestDirectory(ctx, "/Import")
	require.NoError(t, err)
	nested, err := s.ExtendIngestDirectory(ctx, plan, "Notes")
	require.NoError(t, err)
	node, added, resolution, err := s.IngestFileWithMembershipPlanned(ctx, run, nested, "note.txt", fakeHash("b1"), 4, "text/plain", "/source/note.txt", "")
	require.NoError(t, err)
	require.True(t, added)
	resolved, ok := resolution.Rebase(nested).ResolvedID()
	require.True(t, ok)
	require.Equal(t, *node.ParentID, resolved)
	label, err := s.CollectionLabel(ctx, run.ID())
	require.NoError(t, err)
	require.Equal(t, new("Inbox"), label.Label)
	label, err = s.SetCollectionLabel(ctx, run.ID(), label.Revision, new("Reviewed"))
	require.NoError(t, err)
	require.Equal(t, int64(2), label.Revision)
	retry, err := s.BeginIngestWithLabel(ctx, "cli", "Conflicting label", new("Reviewed"))
	require.NoError(t, err)
	missing, err := s.PrepareIngestDirectory(ctx, "/MustNotExist/Child")
	require.NoError(t, err)
	_, _, resolution, err = s.IngestFileWithMembershipPlanned(ctx, retry, missing, "bad.txt", fakeHash("b2"), 8, "text/plain", "/source/bad.txt", "")
	require.ErrorIs(t, err, ErrExists)
	require.True(t, IsInitialIngestAdmissionError(err))
	require.Zero(t, resolution)
	_, err = s.NodeByPath(ctx, "/MustNotExist")
	require.ErrorIs(t, err, ErrNotFound)
	var retained int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM ingests WHERE id=$1`, retry.ID()).Scan(&retained))
	require.Zero(t, retained)
	empty, err := s.BeginIngestWithLabel(ctx, "cli", "Empty folders", new("Empty"))
	require.NoError(t, err)
	require.NoError(t, s.FinalizeIngestDirectories(ctx, empty, []IngestDirectoryPlan{missing}))
	_, err = s.NodeByPath(ctx, "/MustNotExist/Child")
	require.NoError(t, err)
	_, err = s.CollectionLabel(ctx, empty.ID())
	require.ErrorIs(t, err, ErrNotFound)
	// A late receipt failure must discard directories, label and run as well.
	late, err := s.BeginIngestWithLabel(ctx, "cli", "Bad receipt", new("Rejected"))
	require.NoError(t, err)
	latePlan, err := s.PrepareIngestDirectory(ctx, "/LateFailure/Child")
	require.NoError(t, err)
	_, _, resolution, err = s.IngestFileWithMembershipPlanned(ctx, late, latePlan, "bad.txt", node.BlobHash, node.Size+1, "text/plain", "/bad/note.txt", "")
	require.Error(t, err)
	require.Zero(t, resolution)
	_, err = s.NodeByPath(ctx, "/LateFailure")
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM ingests WHERE id=$1`, late.ID()).Scan(&retained))
	require.Zero(t, retained)
}

func TestWatchAndPushPostgres(t *testing.T) {
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "cli", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(ctx, plan)
	require.NoError(t, err)
	run, err := s.BeginIngest(ctx, "watch", "archive")
	require.NoError(t, err)
	node, err := s.IngestFileExact(ctx, run, s.RootID(), "note.txt", fakeHash("c1"), 4, "text/plain", "note.txt", "")
	require.NoError(t, err)
	rebound, err := s.RebindWatchedSource(ctx, "archive", "note.txt", "renamed.txt", node.ID)
	require.NoError(t, err)
	require.Equal(t, node, rebound)
	moved, _, err := s.Move(ctx, node.ID, s.RootID(), "local.txt", node.Revision)
	require.NoError(t, err)
	edited, _, err := s.ReplaceContent(ctx, node.ID, moved.Revision, fakeHash("c2"), 8, "text/plain")
	require.NoError(t, err)
	unchanged, _, changed, err := s.SyncWatchedContent(ctx, "archive", "renamed.txt", fakeHash("c1"), 4, "text/plain")
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, edited, unchanged, "an unchanged source must preserve an independent edit")
	observed, _, changed, err := s.SyncWatchedContent(ctx, "archive", "renamed.txt", fakeHash("c3"), 12, "text/plain")
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "local.txt", observed.Name)
	content := PushContent{RunID: "00000000-0000-4000-8000-000000000001", ParentPath: "/Unused", Name: "note.txt", Hash: fakeHash("c3"), Size: 12, MIMEType: "text/plain"}
	source := PushSource{Name: "archive", Ref: "renamed.txt"}
	skipped, outcome, err := s.AcceptPush(ctx, source, content)
	require.NoError(t, err)
	require.Equal(t, "skipped", outcome)
	require.Equal(t, observed, skipped)
	content.Hash, content.Size = fakeHash("c4"), 16
	updated, outcome, err := s.AcceptPush(ctx, source, content)
	require.NoError(t, err)
	require.Equal(t, "updated", outcome)
	require.Equal(t, node.ID, updated.ID)
	require.Equal(t, "local.txt", updated.Name)
	repeated, outcome, err := s.AcceptPush(ctx, source, content)
	require.NoError(t, err)
	require.Equal(t, "skipped", outcome)
	require.Equal(t, updated, repeated)
	_, err = s.NodeByPath(ctx, "/Unused")
	require.ErrorIs(t, err, ErrNotFound)
	// Reuse the scan for two distinct sources. Cursor pages must use byte order.
	content.ParentPath, content.Name = "/Pushed/Notes", "B.txt"
	first, outcome, err := s.AcceptPush(ctx, PushSource{Name: "archive", Ref: "B.txt"}, content)
	require.NoError(t, err)
	require.Equal(t, "added", outcome)
	content.Name = "_x.txt"
	second, _, err := s.AcceptPush(ctx, PushSource{Name: "archive", Ref: "_x.txt"}, content)
	require.NoError(t, err)
	var cursors []PushCursor
	after := ""
	for {
		page, err := s.PushCursors(ctx, "archive", after, 1)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		cursors = append(cursors, page...)
		after = page[0].Ref
	}
	require.Equal(t, []PushCursor{
		{Ref: "B.txt", NodeID: first.ID, Hash: content.Hash, Size: 16},
		{Ref: "_x.txt", NodeID: second.ID, Hash: content.Hash, Size: 16},
		{Ref: "renamed.txt", NodeID: node.ID, Hash: content.Hash, Size: 16},
	}, cursors)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		high, err := s.auditNodeSequence(ctx, tx)
		if err != nil {
			return err
		}
		return s.validateMetadataState(ctx, tx, high)
	}))
}
