// Package metadata defines Docbank's metadata-v1 logical model and codec.
//
// Before Docbank's first public release this is the only supported metadata
// shape. Older development vaults and streams are intentionally incompatible;
// they are not migration inputs.
package metadata

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	Format        = "docbank-metadata"
	FormatVersion = 1

	typeField            = "type"
	provenanceRecordType = "provenance"

	timestampLayout = "2006-01-02T15:04:05.000000000Z07:00"
)

//go:embed schema.sql
var schemaSQL string

// CreateSchema creates an empty metadata-v1 logical database.
func CreateSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("creating metadata v1 schema: nil database")
	}
	if err := requireForeignKeys(ctx, db); err != nil {
		return fmt.Errorf("creating metadata v1 schema: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("creating metadata v1 schema: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("creating metadata v1 schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("creating metadata v1 schema: %w", err)
	}
	return nil
}

func requireForeignKeys(ctx context.Context, db *sql.DB) error {
	var enabled int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil {
		return fmt.Errorf("checking SQLite foreign keys: %w", err)
	}
	if enabled != 1 {
		return errors.New("SQLite foreign keys are disabled")
	}
	return nil
}

// OpaqueBytes is encoded in JSONL as canonical unpadded base64url. It keeps
// filesystem-derived values byte-exact instead of forcing them through UTF-8.
type OpaqueBytes []byte

func (b OpaqueBytes) MarshalJSON() ([]byte, error) {
	return json.Marshal(base64.RawURLEncoding.EncodeToString(b))
}

func (b *OpaqueBytes) UnmarshalJSON(data []byte) error {
	if b == nil {
		return errors.New("decoding opaque bytes into nil destination")
	}
	var encoded string
	if err := json.Unmarshal(data, &encoded); err != nil {
		return errors.New("opaque bytes must be a base64url string")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decoding opaque bytes: %w", err)
	}
	if base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return errors.New("opaque bytes are not canonical unpadded base64url")
	}
	*b = decoded
	return nil
}

type Header struct {
	Type         string `json:"type"`
	Format       string `json:"format"`
	Version      int    `json:"version"`
	VaultID      string `json:"vault_id"`
	NodeSequence int64  `json:"node_sequence"`
}

type Blob struct {
	Type      string `json:"type"`
	Hash      string `json:"hash"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"created_at"`
}

type Node struct {
	Type             string       `json:"type"`
	ID               int64        `json:"id"`
	ParentID         *int64       `json:"parent_id"`
	Name             OpaqueBytes  `json:"name"`
	Kind             string       `json:"kind"`
	CurrentVersionID *string      `json:"current_version_id"`
	Revision         int64        `json:"revision"`
	CreatedAt        string       `json:"created_at"`
	ModifiedAt       string       `json:"modified_at"`
	TrashedAt        *string      `json:"trashed_at"`
	TrashParent      *int64       `json:"trash_parent"`
	TrashName        *OpaqueBytes `json:"trash_name"`
}

type ContentVersion struct {
	Type                  string  `json:"type"`
	VersionID             string  `json:"version_id"`
	NodeID                int64   `json:"node_id"`
	BlobHash              string  `json:"blob_hash"`
	Size                  int64   `json:"size"`
	MediaType             *string `json:"media_type"`
	RecordedAt            string  `json:"recorded_at"`
	NodeRevision          int64   `json:"node_revision"`
	IntroducedOperationID string  `json:"introduced_operation_id"`
	TransitionKind        string  `json:"transition_kind"`
	SourceVersionID       *string `json:"source_version_id"`
}

type Ingest struct {
	Type       string      `json:"type"`
	IngestID   string      `json:"ingest_id"`
	StartedAt  string      `json:"started_at"`
	SourceKind string      `json:"source_kind"`
	SourceDesc OpaqueBytes `json:"source_desc"`
}

type Provenance struct {
	Type          string       `json:"type"`
	Identity      string       `json:"identity"`
	NodeID        int64        `json:"node_id"`
	IngestID      string       `json:"ingest_id"`
	OriginalPath  *OpaqueBytes `json:"original_path"`
	OriginalMTime *string      `json:"original_mtime"`
	Supersedes    *string      `json:"supersedes"`
}

type Tag struct {
	Type  string `json:"type"`
	TagID string `json:"tag_id"`
	Name  string `json:"name"`
}

type NodeTag struct {
	Type   string `json:"type"`
	NodeID int64  `json:"node_id"`
	TagID  string `json:"tag_id"`
}

type ExtractedText struct {
	Type             string  `json:"type"`
	BlobHash         string  `json:"blob_hash"`
	Extractor        string  `json:"extractor"`
	ExtractorVersion int64   `json:"extractor_version"`
	Status           string  `json:"status"`
	Error            *string `json:"error"`
	Attempts         int64   `json:"attempts"`
	Text             *string `json:"text"`
	ExtractedAt      string  `json:"extracted_at"`
}

// NewUUID returns a canonical random UUIDv4.
func NewUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generating UUID: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return formatUUID(raw), nil
}

func formatUUID(raw [16]byte) string {
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], raw[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], raw[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], raw[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], raw[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], raw[10:16])
	return string(encoded)
}

func parseUUID(value string) ([16]byte, error) {
	var raw [16]byte
	if len(value) != 36 || value[8] != '-' || value[13] != '-' ||
		value[18] != '-' || value[23] != '-' {
		return raw, errors.New("must use canonical lowercase UUID spelling")
	}
	compact := value[0:8] + value[9:13] + value[14:18] + value[19:23] + value[24:36]
	decoded, err := hex.DecodeString(compact)
	if err != nil || hex.EncodeToString(decoded) != compact {
		return raw, errors.New("must use canonical lowercase UUID spelling")
	}
	copy(raw[:], decoded)
	if raw[6]>>4 != 4 || raw[8]>>6 != 2 {
		return raw, errors.New("must be UUIDv4")
	}
	return raw, nil
}

func validateUUID(field, value string) error {
	if _, err := parseUUID(value); err != nil {
		return fmt.Errorf("invalid %s %q: %w", field, value, err)
	}
	return nil
}

func parseDigest(field, value string) ([32]byte, error) {
	var digest [32]byte
	if len(value) != 64 {
		return digest, fmt.Errorf("invalid %s %q: must be lowercase SHA-256", field, value)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(decoded) != value {
		return digest, fmt.Errorf("invalid %s %q: must be lowercase SHA-256", field, value)
	}
	copy(digest[:], decoded)
	return digest, nil
}
