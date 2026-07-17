package store

import (
	"encoding/hex"
	"fmt"
	"time"

	"go.kenn.io/docbank/internal/audit"
)

func provenanceIdentity(value metadataProvenance) (string, error) {
	if value.NodeID <= 0 {
		return "", fmt.Errorf("invalid provenance node ID %d", value.NodeID)
	}
	nodeID := uint64(value.NodeID)
	ingestID, err := audit.UUID(value.IngestID)
	if err != nil {
		return "", err
	}
	originalMTime := audit.Absent()
	if value.OriginalMTime != nil {
		parsed, parseErr := time.Parse(time.RFC3339Nano, *value.OriginalMTime)
		if parseErr != nil {
			return "", fmt.Errorf("parsing original mtime: %w", parseErr)
		}
		originalMTime, err = audit.Timestamp(parsed.UTC().Format(timestampLayout))
		if err != nil {
			return "", err
		}
	}
	supersedes := audit.Absent()
	if value.Supersedes != nil {
		supersedes, err = audit.DigestHex(*value.Supersedes)
		if err != nil {
			return "", err
		}
	}
	digest, err := audit.Hash(audit.Record{Kind: "provenance_identity", Fields: []audit.Field{
		{Name: "node_id", Value: audit.Unsigned(nodeID)},
		{Name: "ingest_id", Value: ingestID},
		{Name: "original_path", Value: audit.Bytes([]byte(value.OriginalPath))},
		{Name: "original_mtime", Value: originalMTime},
		{Name: "supersedes", Value: supersedes},
	}})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest[:]), nil
}
