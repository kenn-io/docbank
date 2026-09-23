package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/docbank/document"
)

const (
	metadataContentMapType         = "content_map"
	metadataContentMapSnapshotType = "content_map_snapshot"
)

type metadataContentMap struct {
	Type             string  `json:"type"`
	ID               string  `json:"id"`
	Owner            string  `json:"owner"`
	Revision         int64   `json:"revision"`
	DefinitionJSON   []byte  `json:"definition_json" format:"byte"`
	DefinitionDigest string  `json:"definition_digest"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
	ArchivedAt       *string `json:"archived_at"`
}

type metadataContentMapSnapshot struct {
	Type             string `json:"type"`
	ID               string `json:"id"`
	MapID            string `json:"map_id"`
	MapRevision      int64  `json:"map_revision"`
	Owner            string `json:"owner"`
	ScopeDigest      string `json:"scope_digest"`
	DefinitionDigest string `json:"definition_digest"`
	SnapshotJSON     []byte `json:"snapshot_json" format:"byte"`
	MemberHash       string `json:"member_hash"`
	CreatedAt        string `json:"created_at"`
}

// Definitions precede their snapshots in metadata v1. Ordering by stable IDs
// makes both ordinary export and backup deterministic.
func exportContentMapMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT id,owner,revision,definition_json,definition_digest,
		created_at,updated_at,archived_at FROM content_maps ORDER BY id`)
	if err != nil {
		return fmt.Errorf("exporting content maps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataContentMap{Type: metadataContentMapType}
		if err := rows.Scan(&record.ID, &record.Owner, &record.Revision, &record.DefinitionJSON,
			&record.DefinitionDigest, &record.CreatedAt, &record.UpdatedAt, &record.ArchivedAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scanning content map: %w", err)
		}
		if err := validateContentMapMetadataRecord(record); err != nil {
			_ = rows.Close()
			return fmt.Errorf("validating content map for export: %w", err)
		}
		if err := write(record); err != nil {
			_ = rows.Close()
			return err
		}
	}
	if err := rowsError("content map", rows); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	snapshots, err := q.QueryContext(ctx, `SELECT id,map_id,map_revision,owner,scope_digest,
		definition_digest,snapshot_json,member_hash,created_at
		FROM content_map_snapshots ORDER BY map_id,created_at,id`)
	if err != nil {
		return fmt.Errorf("exporting content map snapshots: %w", err)
	}
	defer func() { _ = snapshots.Close() }()
	for snapshots.Next() {
		record := metadataContentMapSnapshot{Type: metadataContentMapSnapshotType}
		if err := snapshots.Scan(&record.ID, &record.MapID, &record.MapRevision, &record.Owner,
			&record.ScopeDigest, &record.DefinitionDigest, &record.SnapshotJSON,
			&record.MemberHash, &record.CreatedAt); err != nil {
			return fmt.Errorf("scanning content map snapshot: %w", err)
		}
		if err := validateContentMapSnapshotMetadataRecord(record); err != nil {
			return fmt.Errorf("validating content map snapshot for export: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError("content map snapshot", snapshots)
}

func importContentMapMetadata(ctx context.Context, tx *sql.Tx, record metadataContentMap) error {
	if err := validateContentMapMetadataRecord(record); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO content_maps(
		id,owner,revision,definition_json,definition_digest,created_at,updated_at,archived_at
	) VALUES(?,?,?,?,?,?,?,?)`, record.ID, record.Owner, record.Revision, record.DefinitionJSON,
		record.DefinitionDigest, record.CreatedAt, record.UpdatedAt, record.ArchivedAt)
	return err
}

func importContentMapSnapshotMetadata(ctx context.Context, tx *sql.Tx, record metadataContentMapSnapshot) error {
	if err := validateContentMapSnapshotMetadataRecord(record); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO content_map_snapshots(
		id,map_id,map_revision,owner,scope_digest,definition_digest,snapshot_json,member_hash,created_at
	) VALUES(?,?,?,?,?,?,?,?,?)`, record.ID, record.MapID, record.MapRevision, record.Owner,
		record.ScopeDigest, record.DefinitionDigest, record.SnapshotJSON, record.MemberHash, record.CreatedAt)
	return err
}

func validateContentMapMetadataRecord(record metadataContentMap) error {
	if record.Type != metadataContentMapType || validateUUIDv4(record.ID) != nil ||
		record.Revision < 1 || validateMapAccess(MapAccess{Owner: record.Owner}) != nil ||
		len(record.DefinitionJSON) == 0 || len(record.DefinitionJSON) > 1<<20 {
		return errors.New("invalid content map metadata")
	}
	if err := validateMetadataTime("content map created_at", record.CreatedAt); err != nil {
		return err
	}
	if err := validateMetadataTime("content map updated_at", record.UpdatedAt); err != nil {
		return err
	}
	if record.UpdatedAt < record.CreatedAt {
		return errors.New("content map updated_at precedes created_at")
	}
	if record.ArchivedAt != nil {
		if err := validateMetadataTime("content map archived_at", *record.ArchivedAt); err != nil {
			return err
		}
		if *record.ArchivedAt != record.UpdatedAt {
			return errors.New("content map archive timestamp differs from update timestamp")
		}
	}
	var definition ContentMapDefinition
	if err := json.Unmarshal(record.DefinitionJSON, &definition, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("decoding content map definition: %w", err)
	}
	plan, canonical, err := normalizedMapDefinition(definition)
	if err != nil || !bytes.Equal(canonical, record.DefinitionJSON) || plan.DefinitionDigest != record.DefinitionDigest {
		return errors.New("content map definition is not canonical or its digest differs")
	}
	return nil
}

func validateContentMapSnapshotMetadataRecord(record metadataContentMapSnapshot) error {
	if record.Type != metadataContentMapSnapshotType || validateUUIDv4(record.ID) != nil ||
		validateUUIDv4(record.MapID) != nil || record.MapRevision < 1 ||
		validateMapAccess(MapAccess{Owner: record.Owner}) != nil ||
		!validContentMapDigest(record.ScopeDigest) || !validContentMapDigest(record.DefinitionDigest) ||
		!validContentMapHash(record.MemberHash) ||
		len(record.SnapshotJSON) == 0 || len(record.SnapshotJSON) > 1<<20 {
		return errors.New("invalid content map snapshot metadata")
	}
	if err := validateMetadataTime("content map snapshot created_at", record.CreatedAt); err != nil {
		return err
	}
	var stored contentMapSnapshotStorage
	if err := json.Unmarshal(record.SnapshotJSON, &stored, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("decoding content map snapshot: %w", err)
	}
	snapshot := stored.ContentMapSnapshot
	if snapshot.ID != record.ID || snapshot.MapID != record.MapID ||
		snapshot.MapRevision != record.MapRevision || snapshot.Owner != record.Owner ||
		snapshot.ScopeDigest != record.ScopeDigest || snapshot.DefinitionDigest != record.DefinitionDigest ||
		snapshot.MemberHash != record.MemberHash || snapshot.CreatedAt != record.CreatedAt {
		return errors.New("content map snapshot envelope differs from frozen payload")
	}
	if err := ValidateContentMapSnapshotRecord(snapshot); err != nil {
		return fmt.Errorf("invalid frozen content map snapshot: %w", err)
	}
	if stored.Dependencies == nil || len(*stored.Dependencies) > document.MaxContentMapPins {
		return errors.New("content map snapshot dependency receipt is absent or exceeds bound")
	}
	for _, pin := range *stored.Dependencies {
		if _, err := document.ContentMapPinIdentity(pin); err != nil {
			return fmt.Errorf("invalid frozen content map dependency: %w", err)
		}
	}
	canonical, err := json.Marshal(stored)
	if err != nil || !bytes.Equal(canonical, record.SnapshotJSON) {
		return errors.New("content map snapshot payload is not canonical")
	}
	return nil
}

func validContentMapDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validContentMapHash(strings.TrimPrefix(value, "sha256:"))
}

func validContentMapHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	raw, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(raw) == value
}

func validateContentMapMetadataState(ctx context.Context, q metadataQuerier) error {
	if err := exportContentMapMetadata(ctx, q, func(any) error { return nil }); err != nil {
		return err
	}
	var invalid bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM content_map_snapshots s
		LEFT JOIN content_maps m ON m.id=s.map_id
		WHERE m.id IS NULL OR s.owner<>m.owner OR s.map_revision>m.revision OR
			(s.map_revision=m.revision AND s.definition_digest<>m.definition_digest)
	)`).Scan(&invalid); err != nil {
		return fmt.Errorf("validating content map snapshot relations: %w", err)
	}
	if invalid {
		return errors.New("content map snapshot references absent, foreign, or future definition authority")
	}
	return nil
}
