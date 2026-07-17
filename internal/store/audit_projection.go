package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/docbank/internal/audit"
)

type auditTopologyRow struct {
	id          int64
	parentID    sql.NullInt64
	name        string
	kind        string
	createdAt   string
	modifiedAt  string
	trashedAt   sql.NullString
	trashParent sql.NullInt64
	trashName   sql.NullString
}

func currentAuditTopology(ctx context.Context, tx metadataQuerier) ([]audit.Record, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,parent_id,name,kind,created_at,modified_at,
		trashed_at,trash_parent,trash_name FROM nodes ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("reading current audit topology: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var records []audit.Record
	for rows.Next() {
		var row auditTopologyRow
		if err := rows.Scan(&row.id, &row.parentID, &row.name, &row.kind, &row.createdAt,
			&row.modifiedAt, &row.trashedAt, &row.trashParent, &row.trashName); err != nil {
			return nil, fmt.Errorf("scanning current audit topology: %w", err)
		}
		record, err := topologyRecord(row)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading current audit topology: %w", err)
	}
	return records, nil
}

func topologyRecord(row auditTopologyRow) (audit.Record, error) {
	nodeID, err := positiveAuditNodeID(row.id)
	if err != nil {
		return audit.Record{}, err
	}
	parent := audit.Absent()
	if row.parentID.Valid {
		parentID, idErr := positiveAuditNodeID(row.parentID.Int64)
		if idErr != nil {
			return audit.Record{}, idErr
		}
		parent = audit.Unsigned(parentID)
	}
	createdAt, err := audit.Timestamp(row.createdAt)
	if err != nil {
		return audit.Record{}, fmt.Errorf("encoding topology node %d creation time: %w", row.id, err)
	}
	modifiedAt, err := audit.Timestamp(row.modifiedAt)
	if err != nil {
		return audit.Record{}, fmt.Errorf("encoding topology node %d modification time: %w", row.id, err)
	}
	trashedAt := audit.Absent()
	state := "live"
	if row.trashedAt.Valid {
		state = "trash"
		trashedAt, err = audit.Timestamp(row.trashedAt.String)
		if err != nil {
			return audit.Record{}, fmt.Errorf("encoding topology node %d trash time: %w", row.id, err)
		}
	}
	origin, err := topologyOrigin(row, nodeID)
	if err != nil {
		return audit.Record{}, err
	}
	kindValue, err := audit.Text(row.kind)
	if err != nil {
		return audit.Record{}, err
	}
	stateValue, err := audit.Text(state)
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: "topology_node", Fields: []audit.Field{
		{Name: metadataNodeIDField, Value: audit.Unsigned(nodeID)},
		{Name: "parent_id", Value: parent},
		{Name: "name", Value: audit.Bytes([]byte(row.name))},
		{Name: "node_kind", Value: kindValue},
		{Name: "state", Value: stateValue},
		{Name: "origin", Value: origin},
		{Name: "created_at", Value: createdAt},
		{Name: "modified_at", Value: modifiedAt},
		{Name: "trashed_at", Value: trashedAt},
	}}, nil
}

func topologyOrigin(row auditTopologyRow, nodeID uint64) (audit.Value, error) {
	if !row.trashName.Valid {
		return audit.Absent(), nil
	}
	if row.trashParent.Valid {
		parentID, err := positiveAuditNodeID(row.trashParent.Int64)
		if err != nil {
			return audit.Value{}, err
		}
		return audit.Nested(audit.Record{Kind: "known_origin", Fields: []audit.Field{
			{Name: metadataNodeIDField, Value: audit.Unsigned(nodeID)},
			{Name: "parent_id", Value: audit.Unsigned(parentID)},
			{Name: "name", Value: audit.Bytes([]byte(row.trashName.String))},
		}}), nil
	}
	return audit.Nested(audit.Record{Kind: "unknown_origin", Fields: []audit.Field{
		{Name: metadataNodeIDField, Value: audit.Unsigned(nodeID)},
		{Name: "parent_id", Value: audit.Absent()},
		{Name: "name", Value: audit.Bytes([]byte(row.trashName.String))},
	}}), nil
}

func currentAuditAttachments(ctx context.Context, tx metadataQuerier) ([]audit.Record, error) {
	var records []audit.Record
	appenders := []func(context.Context, metadataQuerier, *[]audit.Record) error{
		appendAuditIngests, appendAuditProvenance, appendAuditTagAssignments, appendAuditTagDefinitions,
	}
	for _, appendRecords := range appenders {
		if err := appendRecords(ctx, tx, &records); err != nil {
			return nil, err
		}
	}
	if err := sortAuditRecordsByCanonicalIdentity(records, attachedAuditIdentity); err != nil {
		return nil, fmt.Errorf("sorting current attached metadata: %w", err)
	}
	return records, nil
}

func appendAuditIngests(ctx context.Context, tx metadataQuerier, records *[]audit.Record) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,started_at,source_kind,source_desc FROM ingests`)
	if err != nil {
		return fmt.Errorf("reading audit ingests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, startedAt, sourceKind, sourceDesc string
		if err := rows.Scan(&id, &startedAt, &sourceKind, &sourceDesc); err != nil {
			return fmt.Errorf("scanning audit ingest: %w", err)
		}
		idValue, err := audit.UUID(id)
		if err != nil {
			return err
		}
		startedValue, err := audit.Timestamp(startedAt)
		if err != nil {
			return err
		}
		kindValue, err := audit.Text(sourceKind)
		if err != nil {
			return err
		}
		*records = append(*records, audit.Record{Kind: metadataIngestType, Fields: []audit.Field{
			{Name: "ingest_id", Value: idValue},
			{Name: "started_at", Value: startedValue},
			{Name: "source_kind", Value: kindValue},
			{Name: "source_desc", Value: audit.Bytes([]byte(sourceDesc))},
		}})
	}
	return rowsError("audit ingests", rows)
}

func appendAuditProvenance(ctx context.Context, tx metadataQuerier, records *[]audit.Record) error {
	rows, err := tx.QueryContext(ctx, `SELECT identity,node_id,ingest_id,original_path,
		original_mtime,supersedes FROM provenance`)
	if err != nil {
		return fmt.Errorf("reading audit provenance: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var identity, ingestID, originalPath string
		var nodeID int64
		var originalMTime, supersedes sql.NullString
		if err := rows.Scan(&identity, &nodeID, &ingestID, &originalPath,
			&originalMTime, &supersedes); err != nil {
			return fmt.Errorf("scanning audit provenance: %w", err)
		}
		record, err := provenanceAuditRecord(identity, nodeID, ingestID, originalPath,
			originalMTime, supersedes)
		if err != nil {
			return err
		}
		*records = append(*records, record)
	}
	return rowsError("audit provenance", rows)
}

func provenanceAuditRecord(
	identity string, nodeID int64, ingestID, originalPath string,
	originalMTime, supersedes sql.NullString,
) (audit.Record, error) {
	auditNodeID, err := positiveAuditNodeID(nodeID)
	if err != nil {
		return audit.Record{}, err
	}
	identityValue, err := audit.DigestHex(identity)
	if err != nil {
		return audit.Record{}, err
	}
	ingestValue, err := audit.UUID(ingestID)
	if err != nil {
		return audit.Record{}, err
	}
	mtimeValue := audit.Absent()
	if originalMTime.Valid {
		parsed, parseErr := time.Parse(time.RFC3339Nano, originalMTime.String)
		if parseErr != nil {
			return audit.Record{}, fmt.Errorf("parsing provenance mtime: %w", parseErr)
		}
		mtimeValue, err = audit.Timestamp(parsed.UTC().Format(timestampLayout))
		if err != nil {
			return audit.Record{}, err
		}
	}
	supersedesValue := audit.Absent()
	if supersedes.Valid {
		supersedesValue, err = audit.DigestHex(supersedes.String)
		if err != nil {
			return audit.Record{}, err
		}
	}
	return audit.Record{Kind: "provenance", Fields: []audit.Field{
		{Name: "identity", Value: identityValue},
		{Name: metadataNodeIDField, Value: audit.Unsigned(auditNodeID)},
		{Name: "ingest_id", Value: ingestValue},
		{Name: "original_path", Value: audit.Bytes([]byte(originalPath))},
		{Name: "original_mtime", Value: mtimeValue},
		{Name: "supersedes", Value: supersedesValue},
	}}, nil
}

func appendAuditTagAssignments(ctx context.Context, tx metadataQuerier, records *[]audit.Record) error {
	rows, err := tx.QueryContext(ctx, `SELECT tag_id,node_id FROM node_tags`)
	if err != nil {
		return fmt.Errorf("reading audit tag assignments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var tagID string
		var nodeID int64
		if err := rows.Scan(&tagID, &nodeID); err != nil {
			return fmt.Errorf("scanning audit tag assignment: %w", err)
		}
		auditNodeID, err := positiveAuditNodeID(nodeID)
		if err != nil {
			return err
		}
		tagValue, err := audit.UUID(tagID)
		if err != nil {
			return err
		}
		*records = append(*records, audit.Record{Kind: "tag_assignment", Fields: []audit.Field{
			{Name: "tag_id", Value: tagValue},
			{Name: metadataNodeIDField, Value: audit.Unsigned(auditNodeID)},
		}})
	}
	return rowsError("audit tag assignments", rows)
}

func appendAuditTagDefinitions(ctx context.Context, tx metadataQuerier, records *[]audit.Record) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,name FROM tags`)
	if err != nil {
		return fmt.Errorf("reading audit tag definitions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var tagID, name string
		if err := rows.Scan(&tagID, &name); err != nil {
			return fmt.Errorf("scanning audit tag definition: %w", err)
		}
		tagValue, err := audit.UUID(tagID)
		if err != nil {
			return err
		}
		nameValue, err := audit.Text(name)
		if err != nil {
			return err
		}
		*records = append(*records, audit.Record{Kind: "tag_definition", Fields: []audit.Field{
			{Name: "tag_id", Value: tagValue}, {Name: "name", Value: nameValue},
		}})
	}
	return rowsError("audit tag definitions", rows)
}

func attachedAuditIdentity(record audit.Record) (audit.Record, error) {
	switch record.Kind {
	case metadataIngestType:
		value, err := auditField(record, "ingest_id")
		return audit.Record{Kind: "ingest_identity", Fields: []audit.Field{{Name: "ingest_id", Value: value}}}, err
	case "provenance":
		value, err := auditField(record, "identity")
		return audit.Record{Kind: "provenance_identity_ref", Fields: []audit.Field{{Name: "identity", Value: value}}}, err
	case "tag_assignment":
		tagID, err := auditField(record, "tag_id")
		if err != nil {
			return audit.Record{}, err
		}
		nodeID, err := auditField(record, metadataNodeIDField)
		return audit.Record{Kind: "tag_assignment_identity", Fields: []audit.Field{
			{Name: "tag_id", Value: tagID}, {Name: metadataNodeIDField, Value: nodeID},
		}}, err
	case "tag_definition":
		value, err := auditField(record, "tag_id")
		return audit.Record{Kind: "tag_definition_identity", Fields: []audit.Field{{Name: "tag_id", Value: value}}}, err
	default:
		return audit.Record{}, fmt.Errorf("record kind %q has no attached-metadata identity", record.Kind)
	}
}

func auditRecordsForNodes(records []audit.Record, members map[uint64]bool) ([]audit.Record, error) {
	selected := make([]audit.Record, 0)
	ingests := make(map[string]bool)
	tags := make(map[string]bool)
	for _, record := range records {
		switch record.Kind {
		case "provenance":
			nodeID, err := auditUnsignedField(record, metadataNodeIDField)
			if err != nil {
				return nil, err
			}
			if members[nodeID] {
				selected = append(selected, record)
				ingestID, err := auditUUIDField(record, "ingest_id")
				if err != nil {
					return nil, err
				}
				ingests[ingestID] = true
			}
		case "tag_assignment":
			nodeID, err := auditUnsignedField(record, metadataNodeIDField)
			if err != nil {
				return nil, err
			}
			if members[nodeID] {
				selected = append(selected, record)
				tagID, err := auditUUIDField(record, "tag_id")
				if err != nil {
					return nil, err
				}
				tags[tagID] = true
			}
		}
	}
	for _, record := range records {
		switch record.Kind {
		case metadataIngestType:
			id, err := auditUUIDField(record, "ingest_id")
			if err != nil {
				return nil, err
			}
			if ingests[id] {
				selected = append(selected, record)
			}
		case "tag_definition":
			id, err := auditUUIDField(record, "tag_id")
			if err != nil {
				return nil, err
			}
			if tags[id] {
				selected = append(selected, record)
			}
		}
	}
	if err := sortAuditRecordsByCanonicalIdentity(selected, attachedAuditIdentity); err != nil {
		return nil, err
	}
	return selected, nil
}

func auditNestedValues(records []audit.Record) []audit.Value {
	values := make([]audit.Value, len(records))
	for index := range records {
		values[index] = audit.Nested(records[index])
	}
	return values
}

func auditMemberSet(values []uint64) map[uint64]bool {
	result := make(map[uint64]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func sortedAuditMembers(members map[uint64]bool) []uint64 {
	result := make([]uint64, 0, len(members))
	for id := range members {
		result = append(result, id)
	}
	slices.Sort(result)
	return result
}
