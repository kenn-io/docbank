package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

func deriveInitialAuditMembers(
	ctx context.Context, tx metadataQuerier, targetNodeID uint64,
) ([]uint64, error) {
	rows, err := loadAuditTopologyRows(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := validateAuditTrashOrigins(rows); err != nil {
		return nil, err
	}
	target, ok := rows[targetNodeID]
	if !ok || target.kind != "dir" || target.trashedAt.Valid {
		return nil, fmt.Errorf("audit enrollment target %d must be a live directory", targetNodeID)
	}
	children := make(map[uint64][]uint64)
	for id, row := range rows {
		if row.parentID.Valid {
			parentID, err := positiveAuditNodeID(row.parentID.Int64)
			if err != nil {
				return nil, err
			}
			children[parentID] = append(children[parentID], id)
		}
	}
	members := map[uint64]bool{targetNodeID: true}
	addDescendants(targetNodeID, rows, children, members, true)
	for changed := true; changed; {
		changed = false
		for id, row := range rows {
			if !row.trashName.Valid || members[id] {
				continue
			}
			adopt := false
			if row.trashParent.Valid {
				parentID, err := positiveAuditNodeID(row.trashParent.Int64)
				if err != nil {
					return nil, err
				}
				adopt = members[parentID]
			}
			if !row.trashParent.Valid && !target.parentID.Valid {
				adopt = true
			}
			if adopt {
				members[id] = true
				addDescendants(id, rows, children, members, false)
				changed = true
			}
		}
	}
	if !target.parentID.Valid && len(members) != len(rows) {
		return nil, fmt.Errorf(
			"root audit enrollment covers %d extant nodes, want %d", len(members), len(rows),
		)
	}
	return sortedAuditMembers(members), nil
}

// validateAuditTrashOrigins follows the historical origin edge for detached
// trash roots and the live parent edge everywhere else. Every chain must end
// at the tree root or at an explicit unknown-origin trash anchor.
func validateAuditTrashOrigins(rows map[uint64]auditTopologyRow) error {
	roots := make(map[uint64]bool)
	for id, row := range rows {
		if row.trashName.Valid {
			roots[id] = true
		}
	}
	for _, start := range sortedAuditMembers(roots) {
		seen := make(map[uint64]bool)
		current := start
		for {
			if seen[current] {
				return fmt.Errorf("audit trash-origin topology contains a cycle at node %d", current)
			}
			seen[current] = true
			row, ok := rows[current]
			if !ok {
				return fmt.Errorf("audit trash-origin topology references missing node %d", current)
			}
			if row.trashName.Valid {
				if !row.trashParent.Valid {
					break
				}
				next, err := positiveAuditNodeID(row.trashParent.Int64)
				if err != nil {
					return err
				}
				current = next
				continue
			}
			if !row.parentID.Valid {
				break
			}
			next, err := positiveAuditNodeID(row.parentID.Int64)
			if err != nil {
				return err
			}
			current = next
		}
	}
	return nil
}

func loadAuditTopologyRows(ctx context.Context, tx metadataQuerier) (map[uint64]auditTopologyRow, error) {
	queryRows, err := tx.QueryContext(ctx, `SELECT id,parent_id,name,kind,created_at,modified_at,
		trashed_at,trash_parent,trash_name FROM nodes ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("reading audit topology rows: %w", err)
	}
	defer func() { _ = queryRows.Close() }()
	result := make(map[uint64]auditTopologyRow)
	for queryRows.Next() {
		var row auditTopologyRow
		if err := queryRows.Scan(&row.id, &row.parentID, &row.name, &row.kind,
			&row.createdAt, &row.modifiedAt, &row.trashedAt, &row.trashParent,
			&row.trashName); err != nil {
			return nil, fmt.Errorf("scanning audit topology row: %w", err)
		}
		id, err := positiveAuditNodeID(row.id)
		if err != nil {
			return nil, err
		}
		result[id] = row
	}
	if err := queryRows.Err(); err != nil {
		return nil, fmt.Errorf("reading audit topology rows: %w", err)
	}
	return result, nil
}

func positiveAuditNodeID(value int64) (uint64, error) {
	if value <= 0 {
		return 0, fmt.Errorf("audit topology contains invalid node ID %d", value)
	}
	return uint64(value), nil
}

func addDescendants(
	root uint64, rows map[uint64]auditTopologyRow, children map[uint64][]uint64,
	members map[uint64]bool, liveOnly bool,
) {
	for _, child := range children[root] {
		if members[child] || (liveOnly && rows[child].trashedAt.Valid) {
			continue
		}
		members[child] = true
		addDescendants(child, rows, children, members, liveOnly)
	}
}

func currentAuditMemberStates(
	ctx context.Context, tx metadataQuerier, members []uint64,
) ([]audit.Record, error) {
	memberSet := auditMemberSet(members)
	rows, err := tx.QueryContext(ctx, `SELECT id,revision,current_version_id FROM nodes ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("reading audit member states: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]audit.Record, 0, len(members))
	for rows.Next() {
		var id, revision int64
		var current sql.NullString
		if err := rows.Scan(&id, &revision, &current); err != nil {
			return nil, fmt.Errorf("scanning audit member state: %w", err)
		}
		nodeID, err := positiveAuditNodeID(id)
		if err != nil {
			return nil, err
		}
		if !memberSet[nodeID] {
			continue
		}
		if revision <= 0 {
			return nil, fmt.Errorf("audit member %d has invalid revision %d", id, revision)
		}
		currentValue := audit.Absent()
		if current.Valid {
			currentValue, err = audit.UUID(current.String)
			if err != nil {
				return nil, err
			}
		}
		result = append(result, audit.Record{Kind: "member_state", Fields: []audit.Field{
			{Name: metadataNodeIDField, Value: audit.Unsigned(nodeID)},
			// The positivity check above makes this conversion lossless.
			{Name: "node_revision", Value: audit.Unsigned(uint64(revision))},
			{Name: "current_version_id", Value: currentValue},
		}})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading audit member states: %w", err)
	}
	if len(result) != len(members) {
		return nil, fmt.Errorf("audit member state cardinality is %d, want %d", len(result), len(members))
	}
	return result, nil
}

func currentAuditVersions(
	ctx context.Context, tx metadataQuerier, members []uint64,
) ([]audit.Record, error) {
	memberSet := auditMemberSet(members)
	rows, err := tx.QueryContext(ctx, `SELECT version_id,node_id,blob_hash,size,mime_type,
		recorded_at,node_revision,introduced_operation_id,transition_kind,source_version_id
		FROM content_versions ORDER BY node_id,version_id`)
	if err != nil {
		return nil, fmt.Errorf("reading audited content versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []audit.Record
	for rows.Next() {
		var versionID, blobHash, recordedAt, operationID, transition string
		var nodeID, size, revision int64
		var mediaType, source sql.NullString
		if err := rows.Scan(&versionID, &nodeID, &blobHash, &size, &mediaType,
			&recordedAt, &revision, &operationID, &transition, &source); err != nil {
			return nil, fmt.Errorf("scanning audited content version: %w", err)
		}
		auditNodeID, err := positiveAuditNodeID(nodeID)
		if err != nil {
			return nil, err
		}
		if !memberSet[auditNodeID] {
			continue
		}
		if size < 0 || revision <= 0 {
			return nil, fmt.Errorf("audited content version %s has invalid size or revision", versionID)
		}
		// The range checks above make both conversions lossless.
		auditSize := uint64(size)
		auditRevision := uint64(revision)
		record, err := contentVersionAuditRecord(versionID, blobHash, recordedAt,
			operationID, transition, auditNodeID, auditSize, auditRevision, mediaType, source)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading audited content versions: %w", err)
	}
	return result, nil
}

func contentVersionAuditRecord(
	versionID, blobHash, recordedAt, operationID, transition string,
	nodeID, size, revision uint64, mediaType, source sql.NullString,
) (audit.Record, error) {
	versionValue, err := audit.UUID(versionID)
	if err != nil {
		return audit.Record{}, err
	}
	blobValue, err := audit.DigestHex(blobHash)
	if err != nil {
		return audit.Record{}, err
	}
	recordedValue, err := audit.Timestamp(recordedAt)
	if err != nil {
		return audit.Record{}, err
	}
	operationValue, err := audit.UUID(operationID)
	if err != nil {
		return audit.Record{}, err
	}
	transitionValue, err := audit.Text(transition)
	if err != nil {
		return audit.Record{}, err
	}
	mediaValue := audit.Absent()
	if mediaType.Valid {
		mediaValue, err = audit.Text(mediaType.String)
		if err != nil {
			return audit.Record{}, err
		}
	}
	sourceValue := audit.Absent()
	if source.Valid {
		sourceValue, err = audit.UUID(source.String)
		if err != nil {
			return audit.Record{}, err
		}
	}
	return audit.Record{Kind: "content_version", Fields: []audit.Field{
		{Name: "version_id", Value: versionValue},
		{Name: metadataNodeIDField, Value: audit.Unsigned(nodeID)},
		{Name: "blob_hash", Value: blobValue},
		{Name: "size", Value: audit.Unsigned(size)},
		{Name: "media_type", Value: mediaValue},
		{Name: "recorded_at", Value: recordedValue},
		{Name: "node_revision", Value: audit.Unsigned(revision)},
		{Name: "introduced_operation_id", Value: operationValue},
		{Name: "transition_kind", Value: transitionValue},
		{Name: "source_version_id", Value: sourceValue},
	}}, nil
}

func initialBaselineTopology(
	topology []audit.Record, members []uint64, generationOperationID string,
) ([]audit.Record, []audit.Record, error) {
	byID := make(map[uint64]audit.Record, len(topology))
	parents := make(map[uint64]*uint64, len(topology))
	originParents := make(map[uint64]*uint64)
	for _, record := range topology {
		id, err := auditUnsignedField(record, metadataNodeIDField)
		if err != nil {
			return nil, nil, err
		}
		byID[id] = record
		parent, err := auditOptionalUnsignedField(record, "parent_id")
		if err != nil {
			return nil, nil, err
		}
		parents[id] = parent
		originValue, err := auditField(record, auditOriginField)
		if err != nil {
			return nil, nil, err
		}
		if originValue.IsAbsent() {
			continue
		}
		origin, ok := originValue.RecordValue()
		if !ok {
			return nil, nil, fmt.Errorf("topology node %d has invalid origin", id)
		}
		if origin.Kind == "known_origin" {
			originParent, err := auditUnsignedField(origin, "parent_id")
			if err != nil {
				return nil, nil, err
			}
			originParents[id] = &originParent
		}
	}
	closure := auditMemberSet(members)
	for _, member := range members {
		addAuditAncestors(member, parents, closure)
		if parent := originParents[member]; parent != nil {
			closure[*parent] = true
			addAuditAncestors(*parent, parents, closure)
		}
	}
	ids := sortedAuditMembers(closure)
	nodes := make([]audit.Record, 0, len(ids))
	memberSet := auditMemberSet(members)
	var witnesses []audit.Record
	generation, err := audit.UUID(generationOperationID)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range ids {
		node, ok := byID[id]
		if !ok {
			return nil, nil, fmt.Errorf("baseline topology dependency node %d is missing", id)
		}
		nodes = append(nodes, node)
		if memberSet[id] {
			continue
		}
		stateDigest, err := audit.Hash(audit.Record{Kind: "witnessed_state", Fields: []audit.Field{
			{Name: "node", Value: audit.Nested(node)},
		}})
		if err != nil {
			return nil, nil, err
		}
		witnesses = append(witnesses, audit.Record{Kind: "witness", Fields: []audit.Field{
			{Name: metadataNodeIDField, Value: audit.Unsigned(id)},
			{Name: "generation_operation_id", Value: generation},
			{Name: "state_digest", Value: audit.Digest(stateDigest)},
		}})
	}
	return nodes, witnesses, nil
}

func addAuditAncestors(id uint64, parents map[uint64]*uint64, closure map[uint64]bool) {
	seen := make(map[uint64]bool)
	for parent := parents[id]; parent != nil && !seen[*parent]; parent = parents[*parent] {
		seen[*parent] = true
		closure[*parent] = true
	}
}

func auditOptionalUnsignedField(record audit.Record, name string) (*uint64, error) {
	value, err := auditField(record, name)
	if err != nil {
		return nil, err
	}
	if value.IsAbsent() {
		return nil, nil //nolint:nilnil // A nil pointer is the canonical absent optional integer.
	}
	result, ok := value.UnsignedValue()
	if !ok {
		return nil, fmt.Errorf("audit field %s.%s is not optional unsigned", record.Kind, name)
	}
	return &result, nil
}

func equalAuditRecordLists(left, right []audit.Record) bool {
	return len(left) == len(right) && slices.EqualFunc(left, right, auditRecordEqual)
}
