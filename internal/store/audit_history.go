package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"

	"go.kenn.io/docbank/internal/audit"
)

const maxAuditHistoryPage = 500

// AuditEvent is one canonical node event projected for human and agent readers.
type AuditEvent struct {
	ID                        string
	OperationID               string
	OperationSequence         int64
	Ordinal                   int64
	NodeID                    int64
	Kind                      string
	ScopeID                   string
	RecordedAt                string
	Origin                    string
	AgentLabel                *string
	PriorNodeRevision         int64
	ResultingNodeRevision     int64
	PriorCurrentVersionID     *string
	ResultingCurrentVersionID *string
	SourceVersionID           *string
	TargetNodeID              *int64
	BaselineDigest            *string
	AttachmentKind            *string
	OldPath                   *string
	NewPath                   *string
}

// AuditEventPage is a stable newest-first page for one audited node.
type AuditEventPage struct {
	Node       Node
	Path       string
	Items      []AuditEvent
	Total      int
	Limit      int
	Cursor     string
	NextCursor string
}

type auditHistoryCursor struct {
	nodeID   int64
	sequence int64
	ordinal  int64
	eventID  string
}

// AuditHistory returns one bounded page for a stable node ID, including trash.
func (s *Store) AuditHistory(
	ctx context.Context, nodeID int64, limit int, cursor string,
) (AuditEventPage, error) {
	return s.auditHistorySnapshot(ctx, limit, cursor, func(tx *sql.Tx) (Node, error) {
		return nodeByIDTx(tx, nodeID)
	})
}

// AuditHistoryPath resolves one live path in the same snapshot as its history.
func (s *Store) AuditHistoryPath(
	ctx context.Context, path string, limit int, cursor string,
) (AuditEventPage, error) {
	return s.auditHistorySnapshot(ctx, limit, cursor, func(tx *sql.Tx) (Node, error) {
		return nodeByPath(ctx, tx, s.rootID, path)
	})
}

func (s *Store) auditHistorySnapshot(
	ctx context.Context,
	limit int,
	cursor string,
	resolve func(*sql.Tx) (Node, error),
) (AuditEventPage, error) {
	if limit < 1 || limit > maxAuditHistoryPage {
		return AuditEventPage{}, fmt.Errorf(
			"audit history limit must be between 1 and %d", maxAuditHistoryPage,
		)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AuditEventPage{}, fmt.Errorf("starting audit-history snapshot: %w", err)
	}
	node, err := resolve(tx)
	if err != nil {
		_ = tx.Rollback()
		return AuditEventPage{}, err
	}
	page, err := auditHistoryPageTx(ctx, tx, node, limit, cursor)
	if err != nil {
		_ = tx.Rollback()
		return AuditEventPage{}, err
	}
	if node.TrashedAt == nil {
		page.Path, err = pathOf(ctx, tx, node.ID)
		if err != nil {
			_ = tx.Rollback()
			return AuditEventPage{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return AuditEventPage{}, fmt.Errorf("closing audit-history snapshot: %w", err)
	}
	return page, nil
}

func auditHistoryPageTx(
	ctx context.Context, tx *sql.Tx, node Node, limit int, rawCursor string,
) (AuditEventPage, error) {
	var memberships int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_memberships WHERE node_id=?`, node.ID,
	).Scan(&memberships); err != nil {
		return AuditEventPage{}, fmt.Errorf("checking audit membership for node %d: %w", node.ID, err)
	}
	if memberships == 0 {
		return AuditEventPage{}, fmt.Errorf("node %d: %w", node.ID, ErrAuditNotEnrolled)
	}
	cursor, err := decodeAuditHistoryCursor(rawCursor, node.ID)
	if err != nil {
		return AuditEventPage{}, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT e.record_json,m.operation_sequence
		FROM audit_records AS e
		JOIN audit_records AS m
		  ON m.kind='canonical_mutation' AND m.operation_id=e.operation_id
		WHERE e.kind='event' AND e.node_id=?
		ORDER BY m.operation_sequence DESC,e.event_ordinal DESC,e.event_id DESC`, node.ID)
	if err != nil {
		return AuditEventPage{}, fmt.Errorf("listing audit history for node %d: %w", node.ID, err)
	}
	defer func() { _ = rows.Close() }()
	page := AuditEventPage{
		Node: node, Items: make([]AuditEvent, 0, limit), Limit: limit, Cursor: rawCursor,
	}
	more := false
	for rows.Next() {
		var raw string
		var sequence int64
		if err := rows.Scan(&raw, &sequence); err != nil {
			return AuditEventPage{}, fmt.Errorf("scanning audit history: %w", err)
		}
		event, err := projectAuditEvent([]byte(raw), sequence)
		if err != nil {
			return AuditEventPage{}, err
		}
		page.Total++
		if !auditEventIsAfterCursor(event, cursor) {
			continue
		}
		if len(page.Items) == limit {
			more = true
			continue
		}
		page.Items = append(page.Items, event)
	}
	if err := rows.Err(); err != nil {
		return AuditEventPage{}, fmt.Errorf("listing audit history for node %d: %w", node.ID, err)
	}
	if more {
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeAuditHistoryCursor(auditHistoryCursor{
			nodeID: node.ID, sequence: last.OperationSequence,
			ordinal: last.Ordinal, eventID: last.ID,
		})
	}
	return page, nil
}

func projectAuditEvent(raw []byte, sequence int64) (AuditEvent, error) {
	wrapper, err := audit.UnmarshalJSONRecord(raw)
	if err != nil {
		return AuditEvent{}, fmt.Errorf("decoding stored audit event: %w", err)
	}
	event, err := auditNestedField(wrapper, auditEventField)
	if err != nil {
		return AuditEvent{}, err
	}
	result := AuditEvent{OperationSequence: sequence}
	if result.ID, err = auditDigestField(event, "event_id"); err != nil {
		return AuditEvent{}, err
	}
	if result.OperationID, err = auditUUIDField(event, auditOperationIDField); err != nil {
		return AuditEvent{}, err
	}
	if result.Ordinal, err = auditInt64UnsignedField(event, auditEventOrdinalField); err != nil {
		return AuditEvent{}, err
	}
	if result.NodeID, err = auditInt64UnsignedField(event, metadataNodeIDField); err != nil {
		return AuditEvent{}, err
	}
	if result.Kind, err = auditTextField(event, "event_kind"); err != nil {
		return AuditEvent{}, err
	}
	if result.ScopeID, err = auditUUIDField(event, auditScopeIDField); err != nil {
		return AuditEvent{}, err
	}
	if result.RecordedAt, err = auditTimestampField(event, auditRecordedAtField); err != nil {
		return AuditEvent{}, err
	}
	if result.Origin, err = auditTextField(event, auditOriginField); err != nil {
		return AuditEvent{}, err
	}
	if result.AgentLabel, err = auditOptionalTextField(event, auditAgentLabelField); err != nil {
		return AuditEvent{}, err
	}
	if result.PriorNodeRevision, err = auditInt64UnsignedField(event, "prior_node_revision"); err != nil {
		return AuditEvent{}, err
	}
	if result.ResultingNodeRevision, err = auditInt64UnsignedField(event, "resulting_node_revision"); err != nil {
		return AuditEvent{}, err
	}
	if result.PriorCurrentVersionID, err = auditOptionalUUIDField(event, "prior_current_version_id"); err != nil {
		return AuditEvent{}, err
	}
	if result.ResultingCurrentVersionID, err = auditOptionalUUIDField(event, "resulting_current_version_id"); err != nil {
		return AuditEvent{}, err
	}
	if result.SourceVersionID, err = auditOptionalUUIDField(event, auditSourceVersionIDField); err != nil {
		return AuditEvent{}, err
	}
	if result.TargetNodeID, err = auditOptionalNodeIDField(event, auditTargetNodeIDField); err != nil {
		return AuditEvent{}, err
	}
	if result.BaselineDigest, err = auditOptionalDigestField(event, auditBaselineDigestField); err != nil {
		return AuditEvent{}, err
	}
	if result.AttachmentKind, err = auditOptionalTextField(event, "attachment_kind"); err != nil {
		return AuditEvent{}, err
	}
	if result.Kind == "node_path" {
		if result.OldPath, err = auditEventPathField(event, auditPreField); err != nil {
			return AuditEvent{}, err
		}
		if result.NewPath, err = auditEventPathField(event, auditPostField); err != nil {
			return AuditEvent{}, err
		}
	}
	return result, nil
}

func auditTimestampField(record audit.Record, name string) (string, error) {
	value, err := auditField(record, name)
	if err != nil {
		return "", err
	}
	result, ok := value.TimestampValue()
	if !ok {
		return "", fmt.Errorf("audit field %s.%s is not a timestamp", record.Kind, name)
	}
	return result, nil
}

func auditOptionalTextField(record audit.Record, name string) (*string, error) {
	value, err := auditField(record, name)
	if err != nil {
		return nil, err
	}
	if value.IsAbsent() {
		return nil, nil //nolint:nilnil // A nil pointer is the canonical absent optional text.
	}
	result, ok := value.TextValue()
	if !ok {
		return nil, fmt.Errorf("audit field %s.%s is not optional text", record.Kind, name)
	}
	return &result, nil
}

func auditOptionalDigestField(record audit.Record, name string) (*string, error) {
	value, err := auditField(record, name)
	if err != nil {
		return nil, err
	}
	if value.IsAbsent() {
		return nil, nil //nolint:nilnil // A nil pointer is the canonical absent optional digest.
	}
	result, ok := value.DigestValue()
	if !ok {
		return nil, fmt.Errorf("audit field %s.%s is not an optional digest", record.Kind, name)
	}
	return &result, nil
}

func auditOptionalNodeIDField(record audit.Record, name string) (*int64, error) {
	value, err := auditField(record, name)
	if err != nil {
		return nil, err
	}
	if value.IsAbsent() {
		return nil, nil //nolint:nilnil // A nil pointer is the canonical absent optional node ID.
	}
	result, ok := value.UnsignedValue()
	if !ok || result == 0 || result > math.MaxInt64 {
		return nil, fmt.Errorf("audit field %s.%s is not an optional node ID", record.Kind, name)
	}
	signed := int64(result)
	return &signed, nil
}

func auditEventPathField(record audit.Record, name string) (*string, error) {
	state, err := auditNestedField(record, name)
	if err != nil {
		return nil, err
	}
	if state.Kind != auditPathStateKind {
		return nil, fmt.Errorf("audit field %s.%s is not a path state", record.Kind, name)
	}
	value, err := auditField(state, auditPathField)
	if err != nil {
		return nil, err
	}
	path, ok := value.BytesValue()
	if !ok {
		return nil, fmt.Errorf("audit field %s.%s is not a byte path", state.Kind, auditPathField)
	}
	text := string(path)
	return &text, nil
}

// ValidateAuditHistoryCursor checks that an opaque cursor belongs to nodeID.
func ValidateAuditHistoryCursor(raw string, nodeID int64) error {
	_, err := decodeAuditHistoryCursor(raw, nodeID)
	return err
}

func decodeAuditHistoryCursor(raw string, nodeID int64) (auditHistoryCursor, error) {
	if raw == "" {
		return auditHistoryCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil {
		return auditHistoryCursor{}, fmt.Errorf("%w: malformed encoding", ErrInvalidAuditCursor)
	}
	parts := strings.Split(string(decoded), ":")
	if len(parts) != 4 {
		return auditHistoryCursor{}, fmt.Errorf("%w: malformed fields", ErrInvalidAuditCursor)
	}
	parsedNodeID, nodeErr := strconv.ParseInt(parts[0], 10, 64)
	sequence, sequenceErr := strconv.ParseInt(parts[1], 10, 64)
	ordinal, ordinalErr := strconv.ParseInt(parts[2], 10, 64)
	if nodeErr != nil || sequenceErr != nil || ordinalErr != nil ||
		parsedNodeID != nodeID || nodeID < 1 || sequence < 1 || ordinal < 0 ||
		validateAuditDigest("history event", parts[3]) != nil {
		return auditHistoryCursor{}, fmt.Errorf("%w: invalid or mismatched fields", ErrInvalidAuditCursor)
	}
	return auditHistoryCursor{
		nodeID: parsedNodeID, sequence: sequence, ordinal: ordinal, eventID: parts[3],
	}, nil
}

func encodeAuditHistoryCursor(cursor auditHistoryCursor) string {
	raw := fmt.Sprintf("%d:%d:%d:%s",
		cursor.nodeID, cursor.sequence, cursor.ordinal, cursor.eventID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func auditEventIsAfterCursor(event AuditEvent, cursor auditHistoryCursor) bool {
	if cursor.nodeID == 0 {
		return true
	}
	return event.OperationSequence < cursor.sequence ||
		(event.OperationSequence == cursor.sequence && event.Ordinal < cursor.ordinal) ||
		(event.OperationSequence == cursor.sequence && event.Ordinal == cursor.ordinal &&
			event.ID < cursor.eventID)
}
