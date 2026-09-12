package store

import (
	"bytes"
	"cmp"
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
	batchTagReceiptV1Version    = 1
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

// BatchTagNodeResultV1 records one target's original fence and committed result.
type BatchTagNodeResultV1 struct {
	NodeID           int64 `json:"node_id"`
	ExpectedRevision int64 `json:"expected_revision"`
	Revision         int64 `json:"revision"`
	Changed          bool  `json:"changed"`
}

// BatchTagReceiptV1 is the frozen persisted v1 wire format. Keep its fields and
// their order unchanged; future receipt formats need their own encoder and decoder.
type BatchTagReceiptV1 struct {
	Version         int                    `json:"version"`
	OperationID     string                 `json:"operation_id"`
	RequestDigest   string                 `json:"request_digest"`
	TagID           string                 `json:"tag_id"`
	Assign          bool                   `json:"assign"`
	TagRevision     int64                  `json:"tag_revision"`
	AssignmentCount int                    `json:"assignment_count"`
	CompletedAt     string                 `json:"completed_at"`
	Nodes           []BatchTagNodeResultV1 `json:"nodes"`
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

type batchTagTargetState struct {
	node     Node
	assigned bool
}

// BatchTags atomically applies one tag assignment choice to an exact bounded
// set of live, revision-fenced nodes. A committed operation ID replays its
// original immutable receipt without consulting current node or tag state.
func (s *Store) BatchTags(ctx context.Context, request BatchTagRequest) (BatchTagReceiptV1, error) {
	targets, digest, err := validateBatchTagRequest(request)
	if err != nil {
		return BatchTagReceiptV1{}, err
	}

	var receipt BatchTagReceiptV1
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
		states, err := loadBatchTagTargetsTx(ctx, tx, request.TagID, targets)
		if err != nil {
			return err
		}
		results, err := s.applyBatchTagsTx(ctx, tx, request, tag, states, nowRFC3339())
		if err != nil {
			return err
		}
		finalTag, err := tagByIDTx(tx, request.TagID)
		if err != nil {
			return err
		}
		receipt = BatchTagReceiptV1{
			Version: batchTagReceiptV1Version, OperationID: request.OperationID,
			RequestDigest: digest, TagID: request.TagID, Assign: request.Assign,
			TagRevision: finalTag.Revision, AssignmentCount: finalTag.AssignmentCount,
			CompletedAt: nowRFC3339(), Nodes: results,
		}
		receiptJSON, err := canonicalBatchTagReceiptV1JSON(receipt)
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
		return BatchTagReceiptV1{}, err
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
	states, err := loadBatchTagTargetsTx(ctx, tx, tagID, targets)
	if err != nil {
		return BatchTagPreview{}, err
	}
	for i, state := range states {
		preview.Nodes[i] = BatchTagPreviewNode{
			NodeID: state.node.ID, Revision: state.node.Revision, Assigned: state.assigned,
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
		return cmp.Compare(a.NodeID, b.NodeID)
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

func loadBatchTagTargetsTx(
	ctx context.Context, tx *sql.Tx, tagID string, targets []BatchTagTarget,
) ([]batchTagTargetState, error) {
	args := make([]any, 1, len(targets)+1)
	args[0] = tagID
	for _, target := range targets {
		args = append(args, target.NodeID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(targets)), ",")
	rows, err := tx.QueryContext(ctx, `SELECT `+nodeCols+`, nt.node_id IS NOT NULL
		FROM `+nodeFrom+` LEFT JOIN node_tags nt ON nt.node_id=n.id AND nt.tag_id=?
		WHERE n.id IN (`+placeholders+`) ORDER BY n.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading batch tag targets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	states := make([]batchTagTargetState, 0, len(targets))
	for rows.Next() {
		var state batchTagTargetState
		n := &state.node
		if err := rows.Scan(&n.ID, &n.ParentID, &n.Name, &n.Kind,
			&n.CurrentVersionID, &n.BlobHash, &n.MD5, &n.Size, &n.MimeType,
			&n.Revision, &n.CreatedAt, &n.ModifiedAt, &n.TrashedAt, &state.assigned); err != nil {
			return nil, fmt.Errorf("scanning batch tag target: %w", err)
		}
		target := targets[len(states)]
		if n.ID != target.NodeID || n.TrashedAt != nil {
			return nil, fmt.Errorf("batch tag target %d is missing or trashed: %w", target.NodeID, ErrNotFound)
		}
		if n.Revision != target.Revision {
			return nil, fmt.Errorf("node %d revision is %d, expected %d: %w",
				n.ID, n.Revision, target.Revision, ErrStaleRevision)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading batch tag targets: %w", err)
	}
	if len(states) != len(targets) {
		return nil, fmt.Errorf("batch tag target %d: %w", targets[len(states)].NodeID, ErrNotFound)
	}
	return states, nil
}

func (s *Store) applyBatchTagsTx(
	ctx context.Context, tx *sql.Tx, request BatchTagRequest, tag Tag,
	states []batchTagTargetState, recordedAt string,
) ([]BatchTagNodeResultV1, error) {
	results := make([]BatchTagNodeResultV1, len(states))
	changedIDs := make([]any, 0, len(states))
	for i, state := range states {
		node := state.node
		changed := state.assigned != request.Assign
		results[i] = BatchTagNodeResultV1{
			NodeID: node.ID, ExpectedRevision: node.Revision, Revision: node.Revision, Changed: changed,
		}
		if changed {
			if node.Revision == math.MaxInt64 {
				return nil, fmt.Errorf("node %d revision cannot advance: %w", node.ID, ErrInvalidBatchTag)
			}
			results[i].Revision++
			changedIDs = append(changedIDs, node.ID)
		}
	}
	if len(changedIDs) == 0 {
		return results, nil
	}
	if tag.Revision > math.MaxInt64-int64(len(changedIDs)) {
		return nil, fmt.Errorf("tag %s revision cannot advance by %d: %w", tag.ID, len(changedIDs), ErrInvalidBatchTag)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(changedIDs)), ",")
	assignmentSQL := `DELETE FROM node_tags WHERE tag_id=? AND node_id IN (` + placeholders + `)`
	if request.Assign {
		assignmentSQL = `INSERT INTO node_tags(node_id,tag_id) SELECT id,? FROM nodes WHERE id IN (` + placeholders + `)`
	}
	if _, err := tx.ExecContext(ctx, assignmentSQL, append([]any{tag.ID}, changedIDs...)...); err != nil {
		return nil, fmt.Errorf("changing batch tag assignments: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET revision=revision+1,modified_at=?
		WHERE id IN (`+placeholders+`)`, append([]any{recordedAt}, changedIDs...)...); err != nil {
		return nil, fmt.Errorf("advancing batch tag nodes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tags SET revision=revision+? WHERE id=?`, len(changedIDs), tag.ID); err != nil {
		return nil, fmt.Errorf("advancing batch tag: %w", err)
	}
	active, err := auditAuthorityActiveTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if active {
		for _, state := range states {
			if state.assigned == request.Assign {
				continue
			}
			if err := s.persistBatchTagAuditTx(ctx, tx, tag.ID, state.node, request.Assign, recordedAt); err != nil {
				return nil, err
			}
		}
	}
	return results, nil
}

func (s *Store) persistBatchTagAuditTx(
	ctx context.Context, tx *sql.Tx, tagID string, prior Node, assign bool, recordedAt string,
) error {
	operationID, err := newUUIDv4()
	if err != nil {
		return err
	}
	authority, scopes, nodeSequence, err := loadAuditedNodeAuthority(ctx, tx, prior.ID)
	if err != nil {
		return err
	}
	result := prior
	result.Revision++
	result.ModifiedAt = recordedAt
	return persistAuditedTagAssignment(ctx, tx, s.vaultID, operationID, recordedAt, nodeSequence,
		authority, scopes, prior, result, tagID, assign)
}

func loadBatchTagReceiptTx(
	ctx context.Context, tx *sql.Tx, operationID string,
) (BatchTagReceiptV1, bool, error) {
	var requestDigest string
	var receiptJSON []byte
	err := tx.QueryRowContext(ctx, `SELECT request_digest,receipt_json
		FROM batch_tag_receipts WHERE operation_id=?`, operationID).Scan(&requestDigest, &receiptJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return BatchTagReceiptV1{}, false, nil
	}
	if err != nil {
		return BatchTagReceiptV1{}, false, fmt.Errorf("loading batch tag receipt %s: %w", operationID, err)
	}
	receipt, err := decodeBatchTagReceiptV1(receiptJSON)
	if err != nil {
		return BatchTagReceiptV1{}, false, fmt.Errorf("validating batch tag receipt %s: %w", operationID, err)
	}
	if receipt.OperationID != operationID || receipt.RequestDigest != requestDigest {
		return BatchTagReceiptV1{}, false, fmt.Errorf("batch tag receipt %s identity does not match its row", operationID)
	}
	return receipt, true, nil
}

func canonicalBatchTagReceiptV1JSON(receipt BatchTagReceiptV1) ([]byte, error) {
	if err := validateBatchTagReceiptV1(receipt); err != nil {
		return nil, err
	}
	return json.Marshal(receipt, json.Deterministic(true))
}

func decodeBatchTagReceiptV1(data []byte) (BatchTagReceiptV1, error) {
	if len(data) == 0 || len(data) > maxBatchTagReceiptJSONBytes {
		return BatchTagReceiptV1{}, fmt.Errorf("receipt JSON must be 1-%d bytes: %w",
			maxBatchTagReceiptJSONBytes, ErrInvalidBatchTag)
	}
	var receipt BatchTagReceiptV1
	if err := json.Unmarshal(data, &receipt, json.RejectUnknownMembers(true)); err != nil {
		return BatchTagReceiptV1{}, fmt.Errorf("decoding receipt: %w", err)
	}
	canonical, err := canonicalBatchTagReceiptV1JSON(receipt)
	if err != nil {
		return BatchTagReceiptV1{}, err
	}
	if !bytes.Equal(data, canonical) {
		return BatchTagReceiptV1{}, errors.New("receipt JSON is not canonical")
	}
	return receipt, nil
}

func validateBatchTagReceiptV1(receipt BatchTagReceiptV1) error {
	if receipt.Version != batchTagReceiptV1Version {
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
