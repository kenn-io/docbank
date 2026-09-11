package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

const (
	maxBatchTagTargets          = 1000
	maxBatchTagReceiptJSONBytes = 1 << 20
	batchTagReceiptVersion      = 1
)

var (
	// ErrInvalidBatchTag means a batch tag request or durable receipt violates
	// the bounded canonical protocol.
	ErrInvalidBatchTag = errors.New("invalid batch tag request")
	// ErrBatchTagOperationConflict means an operation ID already names a
	// different canonical request.
	ErrBatchTagOperationConflict = errors.New("batch tag operation conflicts with existing receipt")
)

// BatchTagTarget identifies one exact node revision in a batch request.
type BatchTagTarget struct {
	NodeID   int64 `json:"node_id"`
	Revision int64 `json:"revision"`
}

// BatchTagRequest names one replay-safe assignment or removal operation.
type BatchTagRequest struct {
	OperationID string           `json:"operation_id"`
	TagID       string           `json:"tag_id"`
	Assign      bool             `json:"assign"`
	Nodes       []BatchTagTarget `json:"nodes"`
}

// BatchTagNodeResult records one target's original fence and committed result.
type BatchTagNodeResult struct {
	NodeID           int64 `json:"node_id"`
	ExpectedRevision int64 `json:"expected_revision"`
	Revision         int64 `json:"revision"`
	Changed          bool  `json:"changed"`
}

// BatchTagReceipt is immutable replay authority for one committed operation.
type BatchTagReceipt struct {
	Version         int                  `json:"version"`
	OperationID     string               `json:"operation_id"`
	RequestDigest   string               `json:"request_digest"`
	TagID           string               `json:"tag_id"`
	Assign          bool                 `json:"assign"`
	TagRevision     int64                `json:"tag_revision"`
	AssignmentCount int                  `json:"assignment_count"`
	CompletedAt     string               `json:"completed_at"`
	Nodes           []BatchTagNodeResult `json:"nodes"`
}

// BatchTagPreviewNode is one exact membership observation.
type BatchTagPreviewNode struct {
	NodeID   int64 `json:"node_id"`
	Revision int64 `json:"revision"`
	Assigned bool  `json:"assigned"`
}

// BatchTagPreview is a bounded, revision-fenced tag membership snapshot.
type BatchTagPreview struct {
	TagID       string                `json:"tag_id"`
	TagRevision int64                 `json:"tag_revision"`
	Nodes       []BatchTagPreviewNode `json:"nodes"`
}

type plannedBatchTagTarget struct {
	target  BatchTagTarget
	node    Node
	changed bool
}

// BatchTags atomically applies one tag assignment choice to an exact bounded
// set of live, revision-fenced nodes. A committed operation ID replays its
// original immutable receipt without consulting current node or tag state.
func (s *Store) BatchTags(ctx context.Context, request BatchTagRequest) (BatchTagReceipt, error) {
	targets, digest, err := validateBatchTagRequest(request)
	if err != nil {
		return BatchTagReceipt{}, err
	}

	var receipt BatchTagReceipt
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		stored, found, err := loadBatchTagReceiptTx(ctx, tx, request.OperationID)
		if err != nil {
			return err
		}
		if found {
			if stored.RequestDigest != digest {
				return fmt.Errorf("operation %s: %w", request.OperationID, ErrBatchTagOperationConflict)
			}
			receipt = stored
			return nil
		}

		tag, err := tagByIDTx(tx, request.TagID)
		if err != nil {
			return err
		}
		planned := make([]plannedBatchTagTarget, len(targets))
		changedCount := int64(0)
		for i, target := range targets {
			node, err := nodeByIDTx(tx, target.NodeID)
			if err != nil {
				return err
			}
			if node.TrashedAt != nil {
				return fmt.Errorf("node %d is trashed: %w", node.ID, ErrNotFound)
			}
			if node.Revision != target.Revision {
				return fmt.Errorf("node %d revision is %d, expected %d: %w",
					node.ID, node.Revision, target.Revision, ErrStaleRevision)
			}
			assigned, err := batchTagAssignedTx(ctx, tx, request.TagID, node.ID)
			if err != nil {
				return err
			}
			changed := assigned != request.Assign
			if changed && node.Revision == math.MaxInt64 {
				return fmt.Errorf("node %d revision cannot advance beyond %d: %w",
					node.ID, node.Revision, ErrInvalidBatchTag)
			}
			planned[i] = plannedBatchTagTarget{target: target, node: node, changed: changed}
			if changed {
				changedCount++
			}
		}
		if changedCount > 0 && tag.Revision > math.MaxInt64-changedCount {
			return fmt.Errorf("tag %s revision cannot advance by %d beyond %d: %w",
				tag.ID, changedCount, math.MaxInt64, ErrInvalidBatchTag)
		}

		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		results := make([]BatchTagNodeResult, len(planned))
		recordedAt := nowRFC3339()
		for i, item := range planned {
			var change TagAssignmentChange
			if active {
				change, err = s.changeAuditedTagAssignmentTx(
					ctx, tx, request.TagID, item.node, item.target.Revision, request.Assign,
				)
			} else {
				change, err = changeTagAssignmentTx(
					ctx, tx, request.TagID, item.node, item.target.Revision, request.Assign, recordedAt,
				)
			}
			if err != nil {
				return err
			}
			if change.Changed != item.changed {
				return fmt.Errorf("node %d assignment changed after batch validation", item.node.ID)
			}
			results[i] = BatchTagNodeResult{
				NodeID: item.node.ID, ExpectedRevision: item.target.Revision,
				Revision: change.Node.Revision, Changed: change.Changed,
			}
		}
		finalTag, err := tagByIDTx(tx, request.TagID)
		if err != nil {
			return err
		}
		receipt = BatchTagReceipt{
			Version: batchTagReceiptVersion, OperationID: request.OperationID,
			RequestDigest: digest, TagID: request.TagID, Assign: request.Assign,
			TagRevision: finalTag.Revision, AssignmentCount: finalTag.AssignmentCount,
			CompletedAt: nowRFC3339(), Nodes: results,
		}
		receiptJSON, err := canonicalBatchTagReceiptJSON(receipt)
		if err != nil {
			return fmt.Errorf("encoding batch tag receipt: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO batch_tag_receipts(
			operation_id,request_digest,receipt_json) VALUES(?,?,?)`,
			request.OperationID, digest, receiptJSON); err != nil {
			return fmt.Errorf("persisting batch tag receipt %s: %w", request.OperationID, err)
		}
		return nil
	})
	if err != nil {
		return BatchTagReceipt{}, err
	}
	return receipt, nil
}

// PreviewBatchTags observes exact assignment membership for one bounded,
// revision-fenced target set from a single read snapshot.
func (s *Store) PreviewBatchTags(
	ctx context.Context, tagID string, nodes []BatchTagTarget,
) (BatchTagPreview, error) {
	targets, _, err := validateBatchTagTargets(tagID, false, nodes)
	if err != nil {
		return BatchTagPreview{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return BatchTagPreview{}, fmt.Errorf("starting batch tag preview: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	tag, err := tagByIDTx(tx, tagID)
	if err != nil {
		return BatchTagPreview{}, err
	}
	preview := BatchTagPreview{
		TagID: tag.ID, TagRevision: tag.Revision,
		Nodes: make([]BatchTagPreviewNode, len(targets)),
	}
	for i, target := range targets {
		node, err := nodeByIDTx(tx, target.NodeID)
		if err != nil {
			return BatchTagPreview{}, err
		}
		if node.TrashedAt != nil {
			return BatchTagPreview{}, fmt.Errorf("node %d is trashed: %w", node.ID, ErrNotFound)
		}
		if node.Revision != target.Revision {
			return BatchTagPreview{}, fmt.Errorf("node %d revision is %d, expected %d: %w",
				node.ID, node.Revision, target.Revision, ErrStaleRevision)
		}
		assigned, err := batchTagAssignedTx(ctx, tx, tagID, node.ID)
		if err != nil {
			return BatchTagPreview{}, err
		}
		preview.Nodes[i] = BatchTagPreviewNode{
			NodeID: node.ID, Revision: node.Revision, Assigned: assigned,
		}
	}
	if err := tx.Commit(); err != nil {
		return BatchTagPreview{}, fmt.Errorf("committing batch tag preview: %w", err)
	}
	return preview, nil
}

func validateBatchTagRequest(request BatchTagRequest) ([]BatchTagTarget, string, error) {
	if err := validateUUIDv4(request.OperationID); err != nil {
		return nil, "", fmt.Errorf("operation_id: %w: %w", err, ErrInvalidBatchTag)
	}
	return validateBatchTagTargets(request.TagID, request.Assign, request.Nodes)
}

func validateBatchTagTargets(
	tagID string, assign bool, nodes []BatchTagTarget,
) ([]BatchTagTarget, string, error) {
	if err := validateUUIDv4(tagID); err != nil {
		return nil, "", fmt.Errorf("tag_id: %w: %w", err, ErrInvalidBatchTag)
	}
	if len(nodes) < 1 || len(nodes) > maxBatchTagTargets {
		return nil, "", fmt.Errorf("batch tag requires 1-%d nodes: %w",
			maxBatchTagTargets, ErrInvalidBatchTag)
	}
	targets := slices.Clone(nodes)
	slices.SortFunc(targets, func(a, b BatchTagTarget) int {
		return intCompare(a.NodeID, b.NodeID)
	})
	for i, target := range targets {
		if target.NodeID < 1 || target.Revision < 1 {
			return nil, "", fmt.Errorf("batch tag node %d requires positive ID and revision: %w",
				target.NodeID, ErrInvalidBatchTag)
		}
		if i > 0 && targets[i-1].NodeID == target.NodeID {
			return nil, "", fmt.Errorf("batch tag repeats node %d: %w",
				target.NodeID, ErrInvalidBatchTag)
		}
	}
	return targets, batchTagDigest(tagID, assign, targets), nil
}

func intCompare(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func batchTagDigest(tagID string, assign bool, targets []BatchTagTarget) string {
	var input strings.Builder
	input.WriteString("docbank-tag-batch-v1\n")
	input.WriteString(tagID)
	input.WriteByte('\n')
	if assign {
		input.WriteString("1\n")
	} else {
		input.WriteString("0\n")
	}
	for _, target := range targets {
		input.WriteString(strconv.FormatInt(target.NodeID, 10))
		input.WriteByte(':')
		input.WriteString(strconv.FormatInt(target.Revision, 10))
		input.WriteByte('\n')
	}
	digest := sha256.Sum256([]byte(input.String()))
	return hex.EncodeToString(digest[:])
}

func batchTagAssignedTx(ctx context.Context, tx *sql.Tx, tagID string, nodeID int64) (bool, error) {
	var assigned bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM node_tags WHERE tag_id=? AND node_id=?)`,
		tagID, nodeID,
	).Scan(&assigned); err != nil {
		return false, fmt.Errorf("checking tag %s assignment to node %d: %w", tagID, nodeID, err)
	}
	return assigned, nil
}

func loadBatchTagReceiptTx(
	ctx context.Context, tx *sql.Tx, operationID string,
) (BatchTagReceipt, bool, error) {
	var requestDigest string
	var receiptJSON []byte
	err := tx.QueryRowContext(ctx, `SELECT request_digest,receipt_json
		FROM batch_tag_receipts WHERE operation_id=?`, operationID).Scan(&requestDigest, &receiptJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return BatchTagReceipt{}, false, nil
	}
	if err != nil {
		return BatchTagReceipt{}, false, fmt.Errorf("loading batch tag receipt %s: %w", operationID, err)
	}
	receipt, err := decodeBatchTagReceipt(receiptJSON)
	if err != nil {
		return BatchTagReceipt{}, false, fmt.Errorf("validating batch tag receipt %s: %w", operationID, err)
	}
	if receipt.OperationID != operationID || receipt.RequestDigest != requestDigest {
		return BatchTagReceipt{}, false, fmt.Errorf("batch tag receipt %s identity does not match its row", operationID)
	}
	return receipt, true, nil
}

func canonicalBatchTagReceiptJSON(receipt BatchTagReceipt) ([]byte, error) {
	if err := validateBatchTagReceipt(receipt); err != nil {
		return nil, err
	}
	return json.Marshal(receipt, json.Deterministic(true))
}

func decodeBatchTagReceipt(data []byte) (BatchTagReceipt, error) {
	if len(data) == 0 || len(data) > maxBatchTagReceiptJSONBytes {
		return BatchTagReceipt{}, fmt.Errorf("receipt JSON must be 1-%d bytes: %w",
			maxBatchTagReceiptJSONBytes, ErrInvalidBatchTag)
	}
	var receipt BatchTagReceipt
	if err := json.Unmarshal(data, &receipt, json.RejectUnknownMembers(true)); err != nil {
		return BatchTagReceipt{}, fmt.Errorf("decoding receipt: %w", err)
	}
	canonical, err := canonicalBatchTagReceiptJSON(receipt)
	if err != nil {
		return BatchTagReceipt{}, err
	}
	if !bytes.Equal(data, canonical) {
		return BatchTagReceipt{}, errors.New("receipt JSON is not canonical")
	}
	return receipt, nil
}

func validateBatchTagReceipt(receipt BatchTagReceipt) error {
	if receipt.Version != batchTagReceiptVersion {
		return fmt.Errorf("unsupported receipt version %d: %w", receipt.Version, ErrInvalidBatchTag)
	}
	if err := validateUUIDv4(receipt.OperationID); err != nil {
		return fmt.Errorf("receipt operation_id: %w: %w", err, ErrInvalidBatchTag)
	}
	if len(receipt.Nodes) < 1 || len(receipt.Nodes) > maxBatchTagTargets {
		return fmt.Errorf("receipt requires 1-%d nodes: %w", maxBatchTagTargets, ErrInvalidBatchTag)
	}
	if receipt.TagRevision < 1 || receipt.AssignmentCount < 0 {
		return fmt.Errorf("receipt has invalid final tag state: %w", ErrInvalidBatchTag)
	}
	if err := validateMetadataTime("batch tag completed_at", receipt.CompletedAt); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBatchTag, err)
	}
	targets := make([]BatchTagTarget, len(receipt.Nodes))
	changedCount := int64(0)
	for i, node := range receipt.Nodes {
		targets[i] = BatchTagTarget{NodeID: node.NodeID, Revision: node.ExpectedRevision}
		if node.Changed {
			changedCount++
			if node.ExpectedRevision == math.MaxInt64 || node.Revision != node.ExpectedRevision+1 {
				return fmt.Errorf("receipt node %d has invalid changed revision: %w",
					node.NodeID, ErrInvalidBatchTag)
			}
		} else if node.Revision != node.ExpectedRevision {
			return fmt.Errorf("receipt node %d has invalid unchanged revision: %w",
				node.NodeID, ErrInvalidBatchTag)
		}
		if i > 0 && receipt.Nodes[i-1].NodeID >= node.NodeID {
			return fmt.Errorf("receipt nodes are not strictly sorted: %w", ErrInvalidBatchTag)
		}
	}
	if receipt.TagRevision <= changedCount {
		return fmt.Errorf("receipt tag revision does not exceed changed node count: %w",
			ErrInvalidBatchTag)
	}
	if receipt.Assign && receipt.AssignmentCount < len(receipt.Nodes) {
		return fmt.Errorf("receipt assignment count omits an assigned target: %w",
			ErrInvalidBatchTag)
	}
	_, digest, err := validateBatchTagTargets(receipt.TagID, receipt.Assign, targets)
	if err != nil {
		return err
	}
	if receipt.RequestDigest != digest {
		return fmt.Errorf("receipt request digest does not match canonical request: %w",
			ErrInvalidBatchTag)
	}
	return nil
}
