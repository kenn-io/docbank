package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
)

// AuditScopeEvidence is one verified scope-chain terminal.
type AuditScopeEvidence struct {
	ID         string
	EntryCount int64
	ChainHead  string
}

// AuditEvidence is the stable authority bundle produced after replay.
type AuditEvidence struct {
	Enabled                    bool
	VaultID                    string
	LineageID                  string
	OperationSequenceHighWater int64
	AllocationEntryCount       int64
	AllocationHead             string
	Scopes                     []AuditScopeEvidence
}

// AuditVerification is one independently replayed audit snapshot plus the
// unique blobs whose bytes permanent history requires.
type AuditVerification struct {
	Evidence       AuditEvidence
	ProtectedBlobs []BlobInfo
	ProtectedBytes int64 // unique raw bytes across ProtectedBlobs
}

// VerifyAudit independently replays canonical audit history against the
// current projections and returns externally recordable terminal evidence.
// Blob bytes are verified by the daemon through physical catalog authority
// after this metadata snapshot succeeds.
func (s *Store) VerifyAudit(ctx context.Context) (result AuditVerification, err error) {
	snapshot, err := s.BeginMetadataSnapshot(ctx)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	if err := snapshot.Export(ctx, io.Discard); err != nil {
		return result, fmt.Errorf("replaying audit metadata: %w", err)
	}
	result.Evidence, err = auditEvidenceSnapshot(ctx, snapshot, s.vaultID)
	if err != nil {
		return result, err
	}
	result.ProtectedBlobs = []BlobInfo{}
	if !result.Evidence.Enabled {
		return result, nil
	}
	rows, err := snapshot.QueryContext(ctx, `
		SELECT DISTINCT b.hash,b.size
		FROM audit_memberships m
		JOIN content_versions v ON v.node_id=m.node_id
		JOIN blobs b ON b.hash=v.blob_hash
		ORDER BY b.hash`)
	if err != nil {
		return result, fmt.Errorf("listing audit-protected blobs: %w", err)
	}
	result.ProtectedBlobs, err = scanBlobInfos(rows, "listing audit-protected blobs")
	if err != nil {
		return result, err
	}
	for _, blob := range result.ProtectedBlobs {
		if blob.Size > math.MaxInt64-result.ProtectedBytes {
			return result, errors.New("audit-protected byte total overflows int64")
		}
		result.ProtectedBytes += blob.Size
	}
	return result, nil
}

func auditEvidenceSnapshot(
	ctx context.Context, snapshot *MetadataSnapshot, vaultID string,
) (AuditEvidence, error) {
	evidence := AuditEvidence{VaultID: vaultID, Scopes: []AuditScopeEvidence{}}
	err := snapshot.QueryRowContext(ctx, `SELECT lineage_id,operation_sequence_high_water,
		allocation_entry_count,allocation_head FROM audit_authority WHERE singleton=1`).Scan(
		&evidence.LineageID, &evidence.OperationSequenceHighWater,
		&evidence.AllocationEntryCount, &evidence.AllocationHead,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return evidence, nil
	}
	if err != nil {
		return AuditEvidence{}, fmt.Errorf("reading verified audit authority: %w", err)
	}
	evidence.Enabled = true
	rows, err := snapshot.QueryContext(ctx,
		`SELECT scope_id,entry_count,chain_head FROM audit_scopes ORDER BY scope_id`)
	if err != nil {
		return AuditEvidence{}, fmt.Errorf("reading verified audit scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var scope AuditScopeEvidence
		if err := rows.Scan(&scope.ID, &scope.EntryCount, &scope.ChainHead); err != nil {
			return AuditEvidence{}, fmt.Errorf("scanning verified audit scope: %w", err)
		}
		evidence.Scopes = append(evidence.Scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return AuditEvidence{}, fmt.Errorf("reading verified audit scopes: %w", err)
	}
	return evidence, nil
}
