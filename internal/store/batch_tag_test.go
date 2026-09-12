package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatchTagsReplayAfterInterveningChange(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.Mkdir(ctx, s.RootID(), "selected")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "Review")
	require.NoError(t, err)
	request := BatchTagRequest{
		OperationID: "11111111-1111-4111-8111-111111111111",
		TagID:       tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: node.ID, Revision: node.Revision}},
	}
	first, err := s.BatchTags(ctx, request)
	require.NoError(t, err)
	require.Len(t, first.Nodes, 1)
	require.True(t, first.Nodes[0].Changed)
	_, err = s.UnassignTag(ctx, tag.ID, node.ID, first.Nodes[0].Revision)
	require.NoError(t, err)
	replay, err := s.BatchTags(ctx, request)
	require.NoError(t, err)
	require.Equal(t, first, replay)
	request.Assign = false
	_, err = s.BatchTags(ctx, request)
	require.ErrorIs(t, err, ErrBatchTagOperationConflict)

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	request.Assign = true
	restoredReplay, err := restored.BatchTags(ctx, request)
	require.NoError(t, err)
	require.Equal(t, first, restoredReplay)
	var reexported bytes.Buffer
	require.NoError(t, restored.ExportMetadata(ctx, &reexported))
	require.Equal(t, exported.Bytes(), reexported.Bytes())
}

func TestBatchTagDigestGoldenVector(t *testing.T) {
	targets := []BatchTagTarget{{NodeID: 9, Revision: 4}, {NodeID: 7, Revision: 3}}
	sorted, digest, err := validateBatchTagTargets(
		"22222222-2222-4222-8222-222222222222", true, targets,
	)
	require.NoError(t, err)
	assert.Equal(t, []BatchTagTarget{{NodeID: 7, Revision: 3}, {NodeID: 9, Revision: 4}}, sorted)
	assert.Equal(t, "9d29429ce1a103db51351b4d136306a00fb8dc77ae392b506ba09e625073c725", digest)
	assert.Equal(t, []BatchTagTarget{{NodeID: 9, Revision: 4}, {NodeID: 7, Revision: 3}}, targets,
		"digest canonicalization must not mutate caller input")
}

func TestBatchTagsRejectsInvalidStructureWithoutWrites(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.Mkdir(ctx, s.RootID(), "selected")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "Bounds")
	require.NoError(t, err)
	valid := BatchTagRequest{
		OperationID: "11111111-1111-4111-8111-111111111111",
		TagID:       tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: node.ID, Revision: node.Revision}},
	}
	tooMany := make([]BatchTagTarget, maxBatchTagTargets+1)
	for i := range tooMany {
		tooMany[i] = BatchTagTarget{NodeID: int64(i + 1), Revision: 1}
	}
	tests := []struct {
		name   string
		mutate func(*BatchTagRequest)
	}{
		{name: "empty", mutate: func(r *BatchTagRequest) { r.Nodes = nil }},
		{name: "oversized", mutate: func(r *BatchTagRequest) { r.Nodes = tooMany }},
		{name: "duplicate", mutate: func(r *BatchTagRequest) { r.Nodes = append(r.Nodes, r.Nodes[0]) }},
		{name: "invalid operation ID", mutate: func(r *BatchTagRequest) { r.OperationID = "not-a-uuid" }},
		{name: "invalid tag ID", mutate: func(r *BatchTagRequest) { r.TagID = "not-a-uuid" }},
		{name: "zero node ID", mutate: func(r *BatchTagRequest) { r.Nodes[0].NodeID = 0 }},
		{name: "zero revision", mutate: func(r *BatchTagRequest) { r.Nodes[0].Revision = 0 }},
		{name: "negative revision", mutate: func(r *BatchTagRequest) { r.Nodes[0].Revision = -1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := valid
			request.Nodes = append([]BatchTagTarget(nil), valid.Nodes...)
			test.mutate(&request)
			_, err := s.BatchTags(ctx, request)
			require.ErrorIs(t, err, ErrInvalidBatchTag)
		})
	}
	var receipts, assignments int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM batch_tag_receipts),
		(SELECT COUNT(*) FROM node_tags)`).Scan(&receipts, &assignments))
	assert.Zero(t, receipts)
	assert.Zero(t, assignments)
	unchanged, err := s.NodeByID(ctx, node.ID)
	require.NoError(t, err)
	assert.Equal(t, node.Revision, unchanged.Revision)
}

func TestBatchTagsMixedChangesNoOpsAndCallerOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.Mkdir(ctx, s.RootID(), "first")
	require.NoError(t, err)
	second, err := s.Mkdir(ctx, s.RootID(), "second")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "Mixed")
	require.NoError(t, err)
	assigned, err := s.AssignTag(ctx, tag.ID, first.ID, first.Revision)
	require.NoError(t, err)
	first, tag = assigned.Node, assigned.Tag
	targets := []BatchTagTarget{
		{NodeID: second.ID, Revision: second.Revision},
		{NodeID: first.ID, Revision: first.Revision},
	}
	wantTargets := append([]BatchTagTarget(nil), targets...)

	request := BatchTagRequest{
		OperationID: "22222222-2222-4222-8222-222222222222",
		TagID:       tag.ID, Assign: true, Nodes: targets,
	}
	receipt, err := s.BatchTags(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, wantTargets, targets)
	assert.Equal(t, tag.Revision+1, receipt.TagRevision)
	assert.Equal(t, 2, receipt.AssignmentCount)
	assert.Equal(t, []BatchTagNodeResultV1{
		{NodeID: first.ID, ExpectedRevision: first.Revision, Revision: first.Revision, Changed: false},
		{NodeID: second.ID, ExpectedRevision: second.Revision, Revision: second.Revision + 1, Changed: true},
	}, receipt.Nodes)
	require.NoError(t, validateMetadataTime("completed_at", receipt.CompletedAt))
	canonical, err := canonicalBatchTagReceiptV1JSON(receipt)
	require.NoError(t, err)
	var stored []byte
	require.NoError(t, s.db.QueryRow(`SELECT receipt_json FROM batch_tag_receipts
		WHERE operation_id=?`, receipt.OperationID).Scan(&stored))
	assert.Equal(t, canonical, stored)
	request.Nodes = []BatchTagTarget{
		{NodeID: first.ID, Revision: first.Revision},
		{NodeID: second.ID, Revision: second.Revision},
	}
	reorderedReplay, err := s.BatchTags(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, receipt, reorderedReplay)
}

func TestBatchTagsAcceptsOneAndOneThousandTargets(t *testing.T) {
	for _, count := range []int{1, maxBatchTagTargets} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			s := newTestStore(t)
			nodes := createBatchTagDirectories(t, s, count)
			tag, err := s.CreateTag(t.Context(), "Bounded")
			require.NoError(t, err)
			targets := make([]BatchTagTarget, len(nodes))
			for i := range nodes {
				targets[len(nodes)-1-i] = BatchTagTarget{NodeID: nodes[i].ID, Revision: nodes[i].Revision}
			}
			receipt, err := s.BatchTags(t.Context(), BatchTagRequest{
				OperationID: fmt.Sprintf("33333333-3333-4333-8333-%012d", count),
				TagID:       tag.ID, Assign: true, Nodes: targets,
			})
			require.NoError(t, err)
			require.Len(t, receipt.Nodes, count)
			assert.Equal(t, count, receipt.AssignmentCount)
			assert.Equal(t, tag.Revision+int64(count), receipt.TagRevision)
			for i, result := range receipt.Nodes {
				assert.Equal(t, nodes[i].ID, result.NodeID)
				assert.True(t, result.Changed)
				assert.Equal(t, nodes[i].Revision+1, result.Revision)
				targets[i] = BatchTagTarget{NodeID: result.NodeID, Revision: result.Revision}
			}
			removed, err := s.BatchTags(t.Context(), BatchTagRequest{
				OperationID: fmt.Sprintf("34343434-3434-4343-8343-%012d", count),
				TagID:       tag.ID, Assign: false, Nodes: targets,
			})
			require.NoError(t, err)
			assert.Zero(t, removed.AssignmentCount)
			assert.Equal(t, tag.Revision+2*int64(count), removed.TagRevision)
			require.NoError(t, s.ValidateMetadata(t.Context()))
		})
	}
}

func TestBatchTagsRejectsLateInvalidTargetAtomically(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *Store, Node) BatchTagTarget
	}{
		{name: "stale", prepare: func(_ *testing.T, _ *Store, node Node) BatchTagTarget {
			return BatchTagTarget{NodeID: node.ID, Revision: node.Revision + 1}
		}},
		{name: "missing", prepare: func(_ *testing.T, _ *Store, node Node) BatchTagTarget {
			return BatchTagTarget{NodeID: node.ID + 10000, Revision: node.Revision}
		}},
		{name: "trashed", prepare: func(t *testing.T, s *Store, node Node) BatchTagTarget {
			t.Helper()
			trashed, _, err := s.Trash(t.Context(), node.ID, node.Revision)
			require.NoError(t, err)
			return BatchTagTarget{NodeID: trashed.ID, Revision: trashed.Revision}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			first, err := s.Mkdir(t.Context(), s.RootID(), "first")
			require.NoError(t, err)
			last, err := s.Mkdir(t.Context(), s.RootID(), "last")
			require.NoError(t, err)
			tag, err := s.CreateTag(t.Context(), "Atomic")
			require.NoError(t, err)
			lastTarget := test.prepare(t, s, last)
			_, err = s.BatchTags(t.Context(), BatchTagRequest{
				OperationID: "44444444-4444-4444-8444-444444444444",
				TagID:       tag.ID, Assign: true,
				Nodes: []BatchTagTarget{
					{NodeID: first.ID, Revision: first.Revision}, lastTarget,
				},
			})
			if test.name == "stale" {
				require.ErrorIs(t, err, ErrStaleRevision)
			} else {
				require.ErrorIs(t, err, ErrNotFound)
			}
			unchanged, readErr := s.NodeByID(t.Context(), first.ID)
			require.NoError(t, readErr)
			assert.Equal(t, first.Revision, unchanged.Revision)
			currentTag, readErr := s.TagByID(t.Context(), tag.ID)
			require.NoError(t, readErr)
			assert.Equal(t, tag, currentTag)
			var receipts, assignments int
			require.NoError(t, s.db.QueryRow(`SELECT
				(SELECT COUNT(*) FROM batch_tag_receipts),
				(SELECT COUNT(*) FROM node_tags WHERE node_id=?)`, first.ID,
			).Scan(&receipts, &assignments))
			assert.Zero(t, receipts)
			assert.Zero(t, assignments)
		})
	}
}

func TestBatchTagsRejectsRevisionOverflowBeforeWrites(t *testing.T) {
	for _, test := range []struct {
		name   string
		update string
	}{
		{name: "node", update: `UPDATE nodes SET revision=? WHERE id=?`},
		{name: "tag", update: `UPDATE tags SET revision=? WHERE id=?`},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			node, err := s.Mkdir(t.Context(), s.RootID(), "overflow")
			require.NoError(t, err)
			tag, err := s.CreateTag(t.Context(), "Overflow")
			require.NoError(t, err)
			id := any(node.ID)
			if test.name == "tag" {
				id = tag.ID
			}
			_, err = s.db.Exec(test.update, int64(math.MaxInt64), id)
			require.NoError(t, err)
			if test.name == "node" {
				node.Revision = math.MaxInt64
			}
			_, err = s.BatchTags(t.Context(), BatchTagRequest{
				OperationID: "55555555-5555-4555-8555-555555555555",
				TagID:       tag.ID, Assign: true,
				Nodes: []BatchTagTarget{{NodeID: node.ID, Revision: node.Revision}},
			})
			require.ErrorIs(t, err, ErrInvalidBatchTag)
			var receipts, assignments int
			require.NoError(t, s.db.QueryRow(`SELECT
				(SELECT COUNT(*) FROM batch_tag_receipts),
				(SELECT COUNT(*) FROM node_tags)`).Scan(&receipts, &assignments))
			assert.Zero(t, receipts)
			assert.Zero(t, assignments)
		})
	}
}

func TestBatchTagsConcurrentSameOperationReturnsOneReceipt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "vault.db")
	firstStore, err := Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, firstStore.Close()) })
	secondStore, err := Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, secondStore.Close()) })
	node, err := firstStore.Mkdir(t.Context(), firstStore.RootID(), "concurrent")
	require.NoError(t, err)
	tag, err := firstStore.CreateTag(t.Context(), "Concurrent")
	require.NoError(t, err)
	request := BatchTagRequest{
		OperationID: "66666666-6666-4666-8666-666666666666",
		TagID:       tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: node.ID, Revision: node.Revision}},
	}
	stores := []*Store{firstStore, secondStore}
	receipts := make([]BatchTagReceiptV1, len(stores))
	errs := make([]error, len(stores))
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			receipts[i], errs[i] = stores[i].BatchTags(context.Background(), request)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, receipts[0], receipts[1])
	var receiptRows, assignments int
	require.NoError(t, firstStore.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM batch_tag_receipts),
		(SELECT COUNT(*) FROM node_tags)`).Scan(&receiptRows, &assignments))
	assert.Equal(t, 1, receiptRows)
	assert.Equal(t, 1, assignments)
}

func TestBatchTagsReplaysAfterRestartDeletionAndNodePurge(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "vault.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	node, err := s.Mkdir(t.Context(), s.RootID(), "historical")
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), "Historical")
	require.NoError(t, err)
	request := BatchTagRequest{
		OperationID: "77777777-7777-4777-8777-777777777777",
		TagID:       tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: node.ID, Revision: node.Revision}},
	}
	receipt, err := s.BatchTags(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s, err = Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	restarted, err := s.BatchTags(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, receipt, restarted)
	unassigned, err := s.UnassignTag(t.Context(), tag.ID, node.ID, receipt.Nodes[0].Revision)
	require.NoError(t, err)
	_, err = s.DeleteTag(t.Context(), tag.ID, unassigned.Tag.Revision)
	require.NoError(t, err)
	trashed, _, err := s.Trash(t.Context(), node.ID, unassigned.Node.Revision)
	require.NoError(t, err)
	assert.Equal(t, node.ID, trashed.ID)
	empty, err := s.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	assert.Equal(t, int64(1), empty.Deleted)
	_, err = s.NodeByID(t.Context(), node.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.TagByID(t.Context(), tag.ID)
	require.ErrorIs(t, err, ErrNotFound)

	historical, err := s.BatchTags(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, receipt, historical)
	request.Assign = false
	_, err = s.BatchTags(t.Context(), request)
	require.ErrorIs(t, err, ErrBatchTagOperationConflict)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	request.Assign = true
	restoredHistorical, err := restored.BatchTags(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, receipt, restoredHistorical)
}

func TestBatchTagsPropagatesCanceledContextAndBackendErrors(t *testing.T) {
	s := newTestStore(t)
	node, err := s.Mkdir(t.Context(), s.RootID(), "cancel")
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), "Cancel")
	require.NoError(t, err)
	request := BatchTagRequest{
		OperationID: "88888888-8888-4888-8888-888888888888",
		TagID:       tag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: node.ID, Revision: node.Revision}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.BatchTags(ctx, request)
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.PreviewBatchTags(ctx, tag.ID, request.Nodes)
	require.ErrorIs(t, err, context.Canceled)

	closed, err := Open(filepath.Join(t.TempDir(), "closed.db"))
	require.NoError(t, err)
	closedNode, err := closed.Mkdir(t.Context(), closed.RootID(), "closed")
	require.NoError(t, err)
	closedTag, err := closed.CreateTag(t.Context(), "Closed")
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	closedRequest := BatchTagRequest{
		OperationID: "99999999-9999-4999-8999-999999999999",
		TagID:       closedTag.ID, Assign: true,
		Nodes: []BatchTagTarget{{NodeID: closedNode.ID, Revision: closedNode.Revision}},
	}
	_, err = closed.BatchTags(t.Context(), closedRequest)
	require.Error(t, err)
	_, err = closed.PreviewBatchTags(t.Context(), closedTag.ID, closedRequest.Nodes)
	require.Error(t, err)
}

func TestAuditedBatchTagsCommitAndReplayCanonicalHistory(t *testing.T) {
	s, tag, report := newAuditedTagStore(t)
	projects, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	request := BatchTagRequest{
		OperationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		TagID:       tag.ID, Assign: true,
		Nodes: []BatchTagTarget{
			{NodeID: report.ID, Revision: report.Revision},
			{NodeID: projects.ID, Revision: projects.Revision},
		},
	}
	receipt, err := s.BatchTags(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, receipt.Nodes, 2)
	assert.True(t, receipt.Nodes[0].Changed)
	assert.True(t, receipt.Nodes[1].Changed)
	assert.Equal(t, "tag_assign", auditEventKindForSequence(t, s, 3))
	var before int64
	require.NoError(t, s.db.QueryRow(
		`SELECT operation_sequence_high_water FROM audit_authority`).Scan(&before))
	assert.Equal(t, int64(3), before)
	require.NoError(t, s.ValidateMetadata(t.Context()))

	replayed, err := s.BatchTags(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, receipt, replayed)
	var after int64
	require.NoError(t, s.db.QueryRow(
		`SELECT operation_sequence_high_water FROM audit_authority`).Scan(&after))
	assert.Equal(t, before, after)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestAuditedBatchTagsRollsBackEarlierTargetWhenLaterTargetIsUnsupported(t *testing.T) {
	s, tag, report := newAuditedTagStore(t)
	empty, err := s.NodeByPath(t.Context(), "/Empty")
	require.NoError(t, err)
	_, err = s.BatchTags(t.Context(), BatchTagRequest{
		OperationID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		TagID:       tag.ID, Assign: true,
		Nodes: []BatchTagTarget{
			{NodeID: report.ID, Revision: report.Revision},
			{NodeID: empty.ID, Revision: empty.Revision},
		},
	})
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	currentReport, err := s.NodeByID(t.Context(), report.ID)
	require.NoError(t, err)
	assert.Equal(t, report.Revision, currentReport.Revision)
	currentTag, err := s.TagByID(t.Context(), tag.ID)
	require.NoError(t, err)
	assert.Equal(t, tag, currentTag)
	var sequence, receipts, assignments int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT operation_sequence_high_water FROM audit_authority),
		(SELECT COUNT(*) FROM batch_tag_receipts),
		(SELECT COUNT(*) FROM node_tags WHERE tag_id=?)`, tag.ID,
	).Scan(&sequence, &receipts, &assignments))
	assert.Equal(t, 1, sequence)
	assert.Zero(t, receipts)
	assert.Zero(t, assignments)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func createBatchTagDirectories(t *testing.T, s *Store, count int) []Node {
	t.Helper()
	result := make([]Node, 0, count)
	recordedAt := time.Date(2026, time.September, 11, 12, 0, 0, 0, time.UTC).Format(timestampLayout)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		for i := range count {
			row, err := tx.ExecContext(t.Context(), `INSERT INTO nodes(
				parent_id,name,kind,created_at,modified_at) VALUES(?,?,'dir',?,?)`,
				s.RootID(), fmt.Sprintf("node-%04d", i), recordedAt, recordedAt)
			if err != nil {
				return err
			}
			id, err := row.LastInsertId()
			if err != nil {
				return err
			}
			node, err := nodeByIDTx(tx, id)
			if err != nil {
				return err
			}
			result = append(result, node)
		}
		return nil
	}))
	return result
}

func TestPreviewBatchTagsExactMembershipAndStaleFences(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.Mkdir(ctx, s.RootID(), "first")
	require.NoError(t, err)
	second, err := s.Mkdir(ctx, s.RootID(), "second")
	require.NoError(t, err)
	third, err := s.Mkdir(ctx, s.RootID(), "third")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "Preview")
	require.NoError(t, err)
	assigned, err := s.AssignTag(ctx, tag.ID, second.ID, second.Revision)
	require.NoError(t, err)
	second = assigned.Node
	tag = assigned.Tag
	targets := []BatchTagTarget{
		{NodeID: third.ID, Revision: third.Revision},
		{NodeID: first.ID, Revision: first.Revision},
		{NodeID: second.ID, Revision: second.Revision},
	}
	wantTargets := append([]BatchTagTarget(nil), targets...)

	preview, err := s.PreviewBatchTags(ctx, tag.ID, targets)
	require.NoError(t, err)
	assert.Equal(t, wantTargets, targets)
	assert.Equal(t, BatchTagPreview{
		TagID: tag.ID, TagRevision: tag.Revision,
		Nodes: []BatchTagPreviewNode{
			{NodeID: first.ID, Revision: first.Revision, Assigned: false},
			{NodeID: second.ID, Revision: second.Revision, Assigned: true},
			{NodeID: third.ID, Revision: third.Revision, Assigned: false},
		},
	}, preview)
	var receipts int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM batch_tag_receipts`).Scan(&receipts))
	assert.Zero(t, receipts)

	targets[2].Revision--
	stale, err := s.PreviewBatchTags(ctx, tag.ID, targets)
	require.ErrorIs(t, err, ErrStaleRevision)
	assert.Zero(t, stale)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM batch_tag_receipts`).Scan(&receipts))
	assert.Zero(t, receipts)
}

func TestPreviewBatchTagsAllNoneAndInvalidTargets(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.Mkdir(ctx, s.RootID(), "first")
	require.NoError(t, err)
	second, err := s.Mkdir(ctx, s.RootID(), "second")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "Tri-state")
	require.NoError(t, err)
	targets := []BatchTagTarget{
		{NodeID: second.ID, Revision: second.Revision},
		{NodeID: first.ID, Revision: first.Revision},
	}
	none, err := s.PreviewBatchTags(ctx, tag.ID, targets)
	require.NoError(t, err)
	for _, node := range none.Nodes {
		assert.False(t, node.Assigned)
	}
	firstAssigned, err := s.AssignTag(ctx, tag.ID, first.ID, first.Revision)
	require.NoError(t, err)
	secondAssigned, err := s.AssignTag(ctx, tag.ID, second.ID, second.Revision)
	require.NoError(t, err)
	targets = []BatchTagTarget{
		{NodeID: first.ID, Revision: firstAssigned.Node.Revision},
		{NodeID: second.ID, Revision: secondAssigned.Node.Revision},
	}
	all, err := s.PreviewBatchTags(ctx, tag.ID, targets)
	require.NoError(t, err)
	for _, node := range all.Nodes {
		assert.True(t, node.Assigned)
	}
	var receipts int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM batch_tag_receipts`).Scan(&receipts))
	assert.Zero(t, receipts)

	for name, invalid := range map[string][]BatchTagTarget{
		"empty":         nil,
		"duplicate":     {targets[0], targets[0]},
		"zero node ID":  {{NodeID: 0, Revision: 1}},
		"zero revision": {{NodeID: first.ID, Revision: 0}},
		"missing":       {{NodeID: second.ID + 10000, Revision: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.PreviewBatchTags(ctx, tag.ID, invalid)
			if name == "missing" {
				require.ErrorIs(t, err, ErrNotFound)
			} else {
				require.ErrorIs(t, err, ErrInvalidBatchTag)
			}
		})
	}
	_, err = s.PreviewBatchTags(ctx, "not-a-uuid", targets)
	require.ErrorIs(t, err, ErrInvalidBatchTag)
	trashed, _, err := s.Trash(ctx, second.ID, secondAssigned.Node.Revision)
	require.NoError(t, err)
	_, err = s.PreviewBatchTags(ctx, tag.ID,
		[]BatchTagTarget{{NodeID: trashed.ID, Revision: trashed.Revision}})
	require.ErrorIs(t, err, ErrNotFound)
}
