package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

func checkAuditedTagWritesPostgres(t *testing.T) {
	t.Helper()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	scope, err := s.Mkdir(ctx, s.RootID(), "scope")
	require.NoError(t, err)
	first, err := s.Mkdir(ctx, scope.ID, "first")
	require.NoError(t, err)
	second, err := s.Mkdir(ctx, scope.ID, "second")
	require.NoError(t, err)
	outside, err := s.Mkdir(ctx, s.RootID(), "outside")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	tag, err := s.CreateTag(ctx, "Review")
	require.NoError(t, err)
	request := BatchTagRequest{OperationID: "11111111-1111-4111-8111-111111111111", TagID: tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: second.ID, Revision: second.Revision}, {NodeID: first.ID, Revision: first.Revision}}}
	preview, err := s.PreviewBatchTags(ctx, tag.ID, request.Nodes)
	require.NoError(t, err)
	require.Len(t, preview.Nodes, 2)
	require.False(t, preview.Nodes[0].Assigned)
	receipt, err := s.BatchTags(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 2, receipt.AssignmentCount)
	checkRetentionMetadata(t, s)
	renamed, err := s.RenameTag(ctx, tag.ID, receipt.TagRevision, "Filed")
	require.NoError(t, err)
	require.Equal(t, 2, renamed.AssignmentCount)
	checkRetentionMetadata(t, s)
	replay, err := s.BatchTags(ctx, request)
	require.NoError(t, err)
	require.Equal(t, receipt, replay)
	current, err := s.NodeByID(ctx, first.ID)
	require.NoError(t, err)
	_, err = s.BatchTags(ctx, BatchTagRequest{OperationID: "22222222-2222-4222-8222-222222222222", TagID: tag.ID, Assign: false,
		Nodes: []BatchTagTarget{{NodeID: first.ID, Revision: current.Revision}, {NodeID: outside.ID, Revision: outside.Revision}}})
	// Unassigning an absent outside target is a no-op, so use assignment to reach its unsupported audit mutation.
	require.NoError(t, err)
	current, err = s.NodeByID(ctx, first.ID)
	require.NoError(t, err)
	before, err := s.AuditStatus(ctx, nil)
	require.NoError(t, err)
	_, err = s.BatchTags(ctx, BatchTagRequest{OperationID: "33333333-3333-4333-8333-333333333333", TagID: tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: first.ID, Revision: current.Revision}, {NodeID: outside.ID, Revision: outside.Revision}}})
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	after, err := s.AuditStatus(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, before, after)
	unchanged, err := s.NodeByID(ctx, first.ID)
	require.NoError(t, err)
	require.Equal(t, current, unchanged)
	var failedReceipts int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM batch_tag_receipts WHERE operation_id=$1`, "33333333-3333-4333-8333-333333333333").Scan(&failedReceipts))
	require.Zero(t, failedReceipts)
	currentTag, err := s.TagByID(ctx, tag.ID)
	require.NoError(t, err)
	_, err = s.DeleteTag(ctx, tag.ID, currentTag.Revision)
	require.NoError(t, err)
	checkRetentionMetadata(t, s)
	replay, err = s.BatchTags(ctx, request)
	require.NoError(t, err)
	require.Equal(t, receipt, replay)
}

func checkTagPhotoWork(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	photo, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("a3"), 4, "image/jpeg")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "Publish")
	require.NoError(t, err)
	var queued int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_xmp_unsettled`).Scan(&queued))
	require.Zero(t, queued)
	changed, err := s.AssignTag(ctx, tag.ID, photo.ID, photo.Revision)
	require.NoError(t, err)
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_xmp_unsettled`).Scan(&queued))
	require.Equal(t, 1, queued)
	_, err = s.UnassignTag(ctx, tag.ID, photo.ID, changed.Node.Revision)
	require.NoError(t, err)
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_xmp_unsettled`).Scan(&queued))
	require.Zero(t, queued)
	current, err := s.NodeByID(ctx, photo.ID)
	require.NoError(t, err)
	_, err = s.BatchTags(ctx, BatchTagRequest{OperationID: "44444444-4444-4444-8444-444444444444", TagID: tag.ID, Assign: true, Nodes: []BatchTagTarget{{NodeID: photo.ID, Revision: current.Revision}}})
	require.NoError(t, err)
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_xmp_unsettled`).Scan(&queued))
	require.Equal(t, 1, queued)
}

func checkBatchTagSnapshotPostgres(t *testing.T) {
	t.Helper()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	node, err := s.Mkdir(ctx, s.RootID(), "selected")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "Review")
	require.NoError(t, err)
	conn, err := s.db.Conn(ctx)
	require.NoError(t, err)
	var cfg *pgx.ConnConfig
	err = conn.Raw(func(c any) error {
		pgConn, ok := c.(*stdlib.Conn)
		require.True(t, ok)
		cfg = pgConn.Conn().Config().Copy()
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	var writeErr error
	cfg.Tracer = &afterFirstSelect{run: func() { _, writeErr = s.AssignTag(ctx, tag.ID, node.ID, node.Revision) }}
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	reader := &Store{db: db}
	nodes := []BatchTagTarget{{NodeID: node.ID, Revision: node.Revision}}
	preview, err := reader.PreviewBatchTags(ctx, tag.ID, nodes)
	require.NoError(t, writeErr)
	require.NoError(t, err)
	require.Equal(t, tag.Revision, preview.TagRevision)
	require.Equal(t, []BatchTagPreviewNode{{NodeID: node.ID, Revision: node.Revision, Assigned: false}}, preview.Nodes)
	_, err = s.PreviewBatchTags(ctx, tag.ID, nodes)
	require.ErrorIs(t, err, ErrStaleRevision)
}

func checkTagPhotoReceiptsPostgres(t *testing.T) {
	t.Helper()
	for _, action := range []string{"rename", "delete", "sidecar batch", "unowned sidecar batch"} {
		t.Run(action, func(t *testing.T) {
			source, asset, raw := xmpSidecarPhoto(t)
			var unowned int64
			if action == "unowned sidecar batch" {
				n, err := source.CreateFile(t.Context(), source.RootID(), "capture.cr2.xmp", fakeHash("e5"), 4, "application/rdf+xml")
				require.NoError(t, err)
				unowned = n.ID
			}
			importXMP(t, source, raw.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating}, "Travel")
			tag, err := source.TagByName(t.Context(), "Travel")
			require.NoError(t, err)
			require.Empty(t, xmpTargets(t, source))
			target, _ := copyRetentionStore(t, source)
			for _, s := range []*Store{source, target} {
				switch action {
				case "rename":
					_, err = s.RenameTag(t.Context(), tag.ID, tag.Revision, "Journey")
				case "delete":
					_, err = s.DeleteTag(t.Context(), tag.ID, tag.Revision)
				default:
					var sidecar int64
					require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT node_id FROM photo_files WHERE asset_id=$1 AND role='sidecar'`, asset.ID).Scan(&sidecar))
					if unowned != 0 {
						sidecar = unowned
					}
					node, readErr := s.NodeByID(t.Context(), sidecar)
					require.NoError(t, readErr)
					_, err = s.BatchTags(t.Context(), BatchTagRequest{OperationID: "55555555-5555-4555-8555-555555555555", TagID: tag.ID, Assign: true, Nodes: []BatchTagTarget{{NodeID: node.ID, Revision: node.Revision}}})
				}
				require.NoError(t, err)
				targets, err := s.PhotoXMPTargetsAfter(t.Context(), "", 10)
				require.NoError(t, err)
				if unowned != 0 {
					// Only occupancy of an unowned sibling enters the original's token.
					require.Empty(t, targets)
				} else {
					require.Contains(t, targets, PhotoSidecarTarget{FileID: raw.ID, NodeID: raw.NodeID, AssetID: asset.ID})
				}
			}
		})
	}
}

func checkTagMailboxGuardPostgres(t *testing.T) {
	t.Helper()
	for _, state := range []string{"queued", "running", "partial"} {
		t.Run(state, func(t *testing.T) {
			source := newTestStore(t)
			ctx := t.Context()
			tag, err := source.CreateTag(ctx, "Project")
			require.NoError(t, err)
			c := MailboxContainerRequest{ID: "source", Owner: "operator", SHA256: fakeHash("aa"), Size: 3, Format: "mbox"}
			_, err = source.BeginMailboxContainer(ctx, c)
			require.NoError(t, err)
			require.NoError(t, source.RecordRenditionBlob(ctx, c.SHA256, c.Size, BlobPhysical{Encoding: "raw", StoredBytes: c.Size, PackEligible: true, Created: true}))
			require.NoError(t, source.PutMailboxChunk(ctx, c.Owner, c.ID, MailboxChunk{Index: 0, SHA256: c.SHA256, Size: c.Size}))
			_, err = source.SealMailboxContainer(ctx, c.Owner, c.ID, c.SHA256, c.Size)
			require.NoError(t, err)
			_, err = source.BeginMailboxJob(ctx, c.Owner, MailboxJobRequest{ID: "job", ContainerID: c.ID, ContainerSHA256: c.SHA256, Settings: MailboxSettings{DestinationID: source.RootID(), LabelTags: map[string]string{"Project": tag.ID}}})
			require.NoError(t, err)
			if state != "queued" {
				job, err := source.ClaimMailboxJob(ctx)
				require.NoError(t, err)
				if state == "partial" {
					require.NoError(t, source.FinishMailboxJob(ctx, job.ID, job.Claim, "partial", "segment limit", false))
				}
			}
			target, _ := copyRetentionStore(t, source)
			_, err = target.DeleteTag(ctx, tag.ID, tag.Revision)
			if state == "partial" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrMailboxConflict)
				current, err := target.TagByID(ctx, tag.ID)
				require.NoError(t, err)
				require.Equal(t, tag, current)
			}
			unrelated, err := target.CreateTag(ctx, "Unrelated")
			require.NoError(t, err)
			_, err = target.DeleteTag(ctx, unrelated.ID, unrelated.Revision)
			require.NoError(t, err)
		})
	}
}

func checkSavedDefinitionRetainedRunsPostgres(t *testing.T) {
	t.Helper()
	source := newTestStore(t)
	ctx := t.Context()
	_, err := source.CreateFile(ctx, source.RootID(), "member.txt", fakeHash("b8"), 1, "text/plain")
	require.NoError(t, err)
	definition, err := source.CreateSavedQuery(ctx, "documents", "", SavedQueryKindQuery, []byte(`{}`))
	require.NoError(t, err)
	snapshots := newQuerySnapshotService(source, querySnapshotServiceOptions{})
	t.Cleanup(func() { require.NoError(t, snapshots.Close()) })
	_, _, err = snapshots.RunSaved(ctx, "operator", definition.ID, definition.Revision, SnapshotRequest{}, func(_ context.Context, write func() error) error { return write() })
	require.NoError(t, err)
	target, _ := copyRetentionStore(t, source)
	var count int
	require.NoError(t, target.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM saved_query_runs WHERE saved_query_id=$1`, definition.ID).Scan(&count))
	require.Equal(t, 1, count)
	_, err = target.DeleteSavedQuery(ctx, definition.ID, definition.Revision)
	require.NoError(t, err)
	require.NoError(t, target.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM saved_query_runs WHERE saved_query_id=$1`, definition.ID).Scan(&count))
	require.Zero(t, count)
}
