package metadata

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// Import transactionally installs a metadata-v1 JSONL stream into an empty
// logical schema.
func Import(ctx context.Context, db *sql.DB, r io.Reader) error {
	if db == nil {
		return errors.New("importing metadata v1: nil database")
	}
	if r == nil {
		return errors.New("importing metadata v1: nil reader")
	}
	if err := requireForeignKeys(ctx, db); err != nil {
		return fmt.Errorf("importing metadata v1: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning metadata v1 import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
		return fmt.Errorf("deferring metadata v1 foreign keys: %w", err)
	}
	if err := requirePristine(ctx, tx); err != nil {
		return err
	}

	dec := json.NewDecoder(bufio.NewReader(r))
	dec.DisallowUnknownFields()
	var rawHeader json.RawMessage
	if err := dec.Decode(&rawHeader); err != nil {
		return fmt.Errorf("decoding metadata v1 header: %w", err)
	}
	if err := validateJSONStrings(rawHeader); err != nil {
		return fmt.Errorf("decoding metadata v1 header: %w", err)
	}
	if err := requireFields(rawHeader,
		[]string{typeField, "format", "version", "vault_id", "node_sequence"}, nil); err != nil {
		return fmt.Errorf("decoding metadata v1 header: %w", err)
	}
	var header Header
	if err := decodeExact(rawHeader, &header); err != nil {
		return fmt.Errorf("decoding metadata v1 header: %w", err)
	}
	if header.Type != "meta" || header.Format != Format ||
		header.Version != FormatVersion || header.NodeSequence <= 0 {
		return fmt.Errorf(
			"unsupported metadata v1 header: type=%q format=%q version=%d node_sequence=%d",
			header.Type, header.Format, header.Version, header.NodeSequence,
		)
	}
	if err := validateUUID("vault ID", header.VaultID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO vault_metadata(singleton,format_version,vault_id) VALUES(1,1,?)`,
		header.VaultID,
	); err != nil {
		return fmt.Errorf("installing metadata v1 header: %w", err)
	}

	for recordNumber := 2; ; recordNumber++ {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("decoding metadata v1 record %d: %w", recordNumber, err)
		}
		if err := validateJSONStrings(raw); err != nil {
			return fmt.Errorf("decoding metadata v1 record %d: %w", recordNumber, err)
		}
		envelope, err := recordType(raw)
		if err != nil {
			return fmt.Errorf("decoding metadata v1 record %d type: %w", recordNumber, err)
		}
		if err := importRecord(ctx, tx, envelope, raw); err != nil {
			return fmt.Errorf(
				"importing metadata v1 record %d (%s): %w",
				recordNumber, envelope, err,
			)
		}
	}
	if err := validateTx(ctx, tx, header.NodeSequence); err != nil {
		return fmt.Errorf("validating metadata v1 import: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sqlite_sequence SET seq = ? WHERE name = 'nodes'`, header.NodeSequence,
	); err != nil {
		return fmt.Errorf("restoring metadata v1 node sequence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing metadata v1 import: %w", err)
	}
	return nil
}

var requiredFields = map[string][]string{
	"blob": {
		typeField, "hash", "size", "created_at",
	},
	"node": {
		typeField, "id", "parent_id", "name", "kind", "current_version_id",
		"revision", "created_at", "modified_at", "trashed_at", "trash_parent", "trash_name",
	},
	"content_version": {
		typeField, "version_id", "node_id", "blob_hash", "size", "media_type",
		"recorded_at", "node_revision", "introduced_operation_id",
		"transition_kind", "source_version_id",
	},
	"ingest": {
		typeField, "ingest_id", "started_at", "source_kind", "source_desc",
	},
	provenanceRecordType: {
		typeField, "identity", "node_id", "ingest_id", "original_path",
		"original_mtime", "supersedes",
	},
	"tag": {
		typeField, "tag_id", "name",
	},
	"node_tag": {
		typeField, "node_id", "tag_id",
	},
	"extracted_text": {
		typeField, "blob_hash", "extractor", "extractor_version", "status",
		"error", "attempts", "text", "extracted_at",
	},
}

var nullableFields = map[string]map[string]struct{}{
	"node": {
		"parent_id": {}, "current_version_id": {}, "trashed_at": {},
		"trash_parent": {}, "trash_name": {},
	},
	"content_version": {"media_type": {}, "source_version_id": {}},
	provenanceRecordType: {
		"original_path": {}, "original_mtime": {}, "supersedes": {},
	},
	"extracted_text": {"error": {}, "text": {}},
}

func importRecord(ctx context.Context, tx *sql.Tx, kind string, raw json.RawMessage) error {
	required, ok := requiredFields[kind]
	if !ok {
		return fmt.Errorf("unknown record type %q", kind)
	}
	if err := requireFields(raw, required, nullableFields[kind]); err != nil {
		return err
	}
	switch kind {
	case "blob":
		var record Blob
		if err := decodeExact(raw, &record); err != nil {
			return err
		}
		if err := validateBlob(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO blobs(hash,size,created_at) VALUES(?,?,?)`,
			record.Hash, record.Size, record.CreatedAt,
		)
		return err
	case "node":
		var record Node
		if err := decodeExact(raw, &record); err != nil {
			return err
		}
		if err := validateNode(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO nodes(
				id,parent_id,name,kind,current_version_id,revision,created_at,modified_at,
				trashed_at,trash_parent,trash_name
			) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			record.ID, record.ParentID, []byte(record.Name), record.Kind,
			record.CurrentVersionID, record.Revision, record.CreatedAt, record.ModifiedAt,
			record.TrashedAt, record.TrashParent, opaqueArgument(record.TrashName),
		)
		return err
	case "content_version":
		var record ContentVersion
		if err := decodeExact(raw, &record); err != nil {
			return err
		}
		if err := validateContentVersion(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO content_versions(
				version_id,node_id,blob_hash,size,media_type,recorded_at,node_revision,
				introduced_operation_id,transition_kind,source_version_id
			) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			record.VersionID, record.NodeID, record.BlobHash, record.Size,
			record.MediaType, record.RecordedAt, record.NodeRevision,
			record.IntroducedOperationID, record.TransitionKind, record.SourceVersionID,
		)
		return err
	case "ingest":
		var record Ingest
		if err := decodeExact(raw, &record); err != nil {
			return err
		}
		if err := validateIngest(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO ingests(ingest_id,started_at,source_kind,source_desc) VALUES(?,?,?,?)`,
			record.IngestID, record.StartedAt, record.SourceKind, []byte(record.SourceDesc),
		)
		return err
	case provenanceRecordType:
		var record Provenance
		if err := decodeExact(raw, &record); err != nil {
			return err
		}
		if err := validateProvenance(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO provenance(
				identity,node_id,ingest_id,original_path,original_mtime,supersedes
			) VALUES(?,?,?,?,?,?)`,
			record.Identity, record.NodeID, record.IngestID,
			opaqueArgument(record.OriginalPath), record.OriginalMTime, record.Supersedes,
		)
		return err
	case "tag":
		var record Tag
		if err := decodeExact(raw, &record); err != nil {
			return err
		}
		if err := validateTag(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO tags(tag_id,name) VALUES(?,?)`, record.TagID, record.Name)
		return err
	case "node_tag":
		var record NodeTag
		if err := decodeExact(raw, &record); err != nil {
			return err
		}
		if err := validateNodeTag(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO node_tags(node_id,tag_id) VALUES(?,?)`, record.NodeID, record.TagID)
		return err
	case "extracted_text":
		var record ExtractedText
		if err := decodeExact(raw, &record); err != nil {
			return err
		}
		if err := validateExtractedText(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO extracted_text(
				blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at
			) VALUES(?,?,?,?,?,?,?,?)`,
			record.BlobHash, record.Extractor, record.ExtractorVersion, record.Status,
			record.Error, record.Attempts, record.Text, record.ExtractedAt,
		)
		return err
	default:
		return fmt.Errorf("unknown record type %q", kind)
	}
}

func decodeExact(raw json.RawMessage, target any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("record contains more than one JSON value")
		}
		return err
	}
	return nil
}

func requireFields(
	raw json.RawMessage, required []string, nullable map[string]struct{},
) error {
	fields, err := objectFields(raw, required)
	if err != nil {
		return err
	}
	for _, field := range required {
		value, ok := fields[field]
		if !ok {
			return fmt.Errorf("metadata v1 record lacks required field %q", field)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			if _, ok := nullable[field]; !ok {
				return fmt.Errorf("metadata v1 field %q cannot be null", field)
			}
		}
	}
	return nil
}

func recordType(raw json.RawMessage) (string, error) {
	fields, err := objectFields(raw, []string{typeField})
	if err != nil {
		return "", err
	}
	value, ok := fields[typeField]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return "", errors.New("metadata v1 record lacks non-null field \"type\"")
	}
	var kind string
	if err := json.Unmarshal(value, &kind); err != nil {
		return "", err
	}
	return kind, nil
}

func objectFields(raw json.RawMessage, known []string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	first, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := first.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("metadata v1 record must be a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("metadata v1 object key is not a string")
		}
		for _, canonical := range known {
			if strings.EqualFold(key, canonical) && key != canonical {
				return nil, fmt.Errorf(
					"metadata v1 field %q is a case alias of %q", key, canonical,
				)
			}
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("metadata v1 record contains duplicate field %q", key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	last, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := last.(json.Delim); !ok || delim != '}' {
		return nil, errors.New("metadata v1 record has invalid object terminator")
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("metadata v1 record contains more than one JSON value")
		}
		return nil, err
	}
	return fields, nil
}

func validateBlob(record Blob) error {
	if record.Type != "blob" || record.Size < 0 {
		return errors.New("invalid blob record")
	}
	if _, err := parseDigest("blob hash", record.Hash); err != nil {
		return err
	}
	return validateTimestamp("blob created_at", record.CreatedAt)
}

func validateNode(record Node) error {
	if record.Type != "node" || record.ID <= 0 || record.Revision <= 0 {
		return errors.New("invalid node record")
	}
	if err := validateTimestamp("node created_at", record.CreatedAt); err != nil {
		return err
	}
	if err := validateTimestamp("node modified_at", record.ModifiedAt); err != nil {
		return err
	}
	if record.TrashedAt != nil {
		if err := validateTimestamp("node trashed_at", *record.TrashedAt); err != nil {
			return err
		}
	}
	if record.ParentID == nil {
		if len(record.Name) != 0 || record.Kind != "dir" || record.CurrentVersionID != nil ||
			record.TrashedAt != nil || record.TrashParent != nil || record.TrashName != nil {
			return errors.New("invalid root node record")
		}
		return nil
	}
	if *record.ParentID <= 0 || !validComponent(record.Name) {
		return errors.New("invalid non-root node identity")
	}
	switch record.Kind {
	case "dir":
		if record.CurrentVersionID != nil {
			return errors.New("directory carries current content version")
		}
	case "file":
		if record.CurrentVersionID == nil {
			return errors.New("file lacks current content version")
		}
		if err := validateUUID("current version ID", *record.CurrentVersionID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid node kind %q", record.Kind)
	}
	if record.TrashParent != nil && record.TrashName == nil {
		return errors.New("trash parent requires trash name")
	}
	if (record.TrashParent != nil || record.TrashName != nil) && record.TrashedAt == nil {
		return errors.New("trash coordinates require trashed_at")
	}
	if record.TrashParent != nil && *record.TrashParent <= 0 {
		return errors.New("invalid trash parent")
	}
	if record.TrashName != nil && !validComponent(*record.TrashName) {
		return errors.New("invalid trash name")
	}
	return nil
}

func validateContentVersion(record ContentVersion) error {
	if record.Type != "content_version" || record.NodeID <= 0 || record.Size < 0 {
		return errors.New("invalid content version record")
	}
	if err := validateUUID("content version ID", record.VersionID); err != nil {
		return err
	}
	if _, err := parseDigest("content version blob hash", record.BlobHash); err != nil {
		return err
	}
	if err := validateTimestamp("content version recorded_at", record.RecordedAt); err != nil {
		return err
	}
	if record.MediaType != nil && !utf8.ValidString(*record.MediaType) {
		return errors.New("content version media type is not valid UTF-8")
	}
	if record.NodeRevision <= 0 {
		return errors.New("content version has invalid node revision")
	}
	if err := validateUUID("introduced operation ID", record.IntroducedOperationID); err != nil {
		return err
	}
	switch record.TransitionKind {
	case "content_create", "content_replace":
		if record.SourceVersionID != nil {
			return errors.New("create/replace content version carries source")
		}
	case "content_revert":
		if record.SourceVersionID == nil {
			return errors.New("revert content version lacks source")
		}
		if err := validateUUID("source version ID", *record.SourceVersionID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid transition kind %q", record.TransitionKind)
	}
	return nil
}

func validateIngest(record Ingest) error {
	if record.Type != "ingest" || record.SourceKind == "" || !utf8.ValidString(record.SourceKind) {
		return errors.New("invalid ingest record")
	}
	if err := validateUUID("ingest ID", record.IngestID); err != nil {
		return err
	}
	return validateTimestamp("ingest started_at", record.StartedAt)
}

func validateProvenance(record Provenance) error {
	if record.Type != provenanceRecordType || record.NodeID <= 0 {
		return errors.New("invalid provenance record")
	}
	if _, err := parseDigest("provenance identity", record.Identity); err != nil {
		return err
	}
	if err := validateUUID("provenance ingest ID", record.IngestID); err != nil {
		return err
	}
	if record.OriginalMTime != nil {
		if err := validateTimestamp("provenance original_mtime", *record.OriginalMTime); err != nil {
			return err
		}
	}
	if record.Supersedes != nil {
		if _, err := parseDigest("provenance supersedes", *record.Supersedes); err != nil {
			return err
		}
	}
	identity, err := ProvenanceIdentity(record)
	if err != nil {
		return err
	}
	if identity != record.Identity {
		return errors.New("provenance identity does not match canonical fields")
	}
	return nil
}

func validateTag(record Tag) error {
	if record.Type != "tag" || record.Name == "" || !utf8.ValidString(record.Name) {
		return errors.New("invalid tag record")
	}
	return validateUUID("tag ID", record.TagID)
}

func validateNodeTag(record NodeTag) error {
	if record.Type != "node_tag" || record.NodeID <= 0 {
		return errors.New("invalid node tag record")
	}
	return validateUUID("node tag ID", record.TagID)
}

func validateExtractedText(record ExtractedText) error {
	if record.Type != "extracted_text" || record.Extractor == "" ||
		record.ExtractorVersion < 0 || record.Attempts < 0 ||
		(record.Status != "ok" && record.Status != "failed") ||
		!utf8.ValidString(record.Extractor) || !validOptionalText(record.Error) ||
		!validOptionalText(record.Text) {
		return errors.New("invalid extracted text record")
	}
	if _, err := parseDigest("extracted text blob hash", record.BlobHash); err != nil {
		return err
	}
	return validateTimestamp("extracted text extracted_at", record.ExtractedAt)
}

func validateTimestamp(field, value string) error {
	parsed, err := time.Parse(timestampLayout, value)
	if err != nil || parsed.UTC().Format(timestampLayout) != value {
		return fmt.Errorf("invalid %s %q: expected canonical UTC nanoseconds", field, value)
	}
	return nil
}

func validComponent(value []byte) bool {
	if len(value) == 0 || bytes.Equal(value, []byte(".")) || bytes.Equal(value, []byte("..")) {
		return false
	}
	return !bytes.ContainsAny(value, "\x00/")
}

func opaqueArgument(value *OpaqueBytes) any {
	if value == nil {
		return nil
	}
	return []byte(*value)
}

func requirePristine(ctx context.Context, tx *sql.Tx) error {
	var count int64
	if err := tx.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM vault_metadata) +
		  (SELECT COUNT(*) FROM blobs) +
		  (SELECT COUNT(*) FROM nodes) +
		  (SELECT COUNT(*) FROM content_versions) +
		  (SELECT COUNT(*) FROM ingests) +
		  (SELECT COUNT(*) FROM provenance) +
		  (SELECT COUNT(*) FROM tags) +
		  (SELECT COUNT(*) FROM node_tags) +
		  (SELECT COUNT(*) FROM extracted_text)
	`).Scan(&count); err != nil {
		return fmt.Errorf("checking metadata v1 import target: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("metadata v1 import target is not pristine: rows=%d", count)
	}
	return nil
}

func validOptionalText(value *string) bool {
	return value == nil || utf8.ValidString(*value)
}

// validateJSONStrings rejects invalid UTF-8 and unpaired UTF-16 surrogate
// escapes before encoding/json can replace them with U+FFFD.
func validateJSONStrings(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("JSON contains invalid UTF-8")
	}
	for offset := 0; offset < len(raw); offset++ {
		if raw[offset] != '"' {
			continue
		}
		offset++
		for ; offset < len(raw) && raw[offset] != '"'; offset++ {
			if raw[offset] != '\\' {
				continue
			}
			offset++
			if offset >= len(raw) {
				return errors.New("JSON string ends after escape")
			}
			if raw[offset] != 'u' {
				continue
			}
			code, ok := decodeHexCodeUnit(raw, offset+1)
			if !ok {
				return errors.New("JSON string contains invalid Unicode escape")
			}
			offset += 4
			switch {
			case code >= 0xd800 && code <= 0xdbff:
				if offset+6 >= len(raw) || raw[offset+1] != '\\' || raw[offset+2] != 'u' {
					return errors.New("JSON string contains unpaired high surrogate")
				}
				low, ok := decodeHexCodeUnit(raw, offset+3)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return errors.New("JSON string contains unpaired high surrogate")
				}
				offset += 6
			case code >= 0xdc00 && code <= 0xdfff:
				return errors.New("JSON string contains unpaired low surrogate")
			}
		}
	}
	return nil
}

func decodeHexCodeUnit(raw []byte, start int) (uint16, bool) {
	if start+4 > len(raw) {
		return 0, false
	}
	var value uint16
	for _, char := range raw[start : start+4] {
		value <<= 4
		switch {
		case char >= '0' && char <= '9':
			value |= uint16(char - '0')
		case char >= 'a' && char <= 'f':
			value |= uint16(char-'a') + 10
		case char >= 'A' && char <= 'F':
			value |= uint16(char-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}
