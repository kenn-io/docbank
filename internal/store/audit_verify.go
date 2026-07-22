package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"

	"go.kenn.io/docbank/internal/audit"
)

// MaxAuditEvidenceScopes bounds caller-supplied prefix work independently of
// the HTTP body limit. Enrollment enforces the same limit so every valid vault
// can produce a terminal evidence bundle.
const MaxAuditEvidenceScopes = 1000

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

// AuditEvidenceProblem explains why current authority does not extend one
// externally recorded evidence bundle.
type AuditEvidenceProblem struct {
	Code    string
	ScopeID string
	Message string
}

// AuditEvidenceCheck is the exact-prefix proof for externally recorded audit
// evidence. Extends is true exactly when Problems is empty.
type AuditEvidenceCheck struct {
	Extends  bool
	Problems []AuditEvidenceProblem
}

// AuditVerification is one independently replayed audit snapshot plus the
// unique blobs whose bytes permanent history requires.
type AuditVerification struct {
	Evidence       AuditEvidence
	EvidenceCheck  *AuditEvidenceCheck
	ProtectedBlobs []BlobInfo
	ProtectedBytes int64 // unique raw bytes across ProtectedBlobs
}

// VerifyAudit independently replays canonical audit history against the
// current projections and returns externally recordable terminal evidence.
// Blob bytes are verified by the daemon through physical catalog authority
// after this metadata snapshot succeeds.
func (s *Store) VerifyAudit(
	ctx context.Context, expected *AuditEvidence,
) (result AuditVerification, err error) {
	if expected != nil {
		if err := ValidateAuditEvidence(*expected); err != nil {
			return result, fmt.Errorf("invalid expected audit evidence: %w", err)
		}
	}
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
	if expected != nil {
		check, err := checkAuditEvidencePrefix(ctx, snapshot, result.Evidence, *expected)
		if err != nil {
			return result, err
		}
		result.EvidenceCheck = &check
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

// ValidateAuditEvidence rejects evidence that cannot represent a terminal
// allocation lineage plus a stable, sorted set of scope terminals.
func ValidateAuditEvidence(evidence AuditEvidence) error {
	if err := validateUUIDv4(evidence.VaultID); err != nil {
		return fmt.Errorf("vault ID: %w", err)
	}
	if err := validateUUIDv4(evidence.LineageID); err != nil {
		return fmt.Errorf("lineage ID: %w", err)
	}
	if evidence.OperationSequenceHighWater < 1 ||
		evidence.OperationSequenceHighWater != evidence.AllocationEntryCount {
		return errors.New("allocation count must equal the positive operation high-water mark")
	}
	if _, err := audit.DigestHex(evidence.AllocationHead); err != nil {
		return fmt.Errorf("allocation head: %w", err)
	}
	if len(evidence.Scopes) == 0 || len(evidence.Scopes) > MaxAuditEvidenceScopes {
		return fmt.Errorf("audit evidence must contain between 1 and %d scopes",
			MaxAuditEvidenceScopes)
	}
	previousScopeID := ""
	for index, scope := range evidence.Scopes {
		if err := validateUUIDv4(scope.ID); err != nil {
			return fmt.Errorf("scope %d ID: %w", index, err)
		}
		if scope.EntryCount < 1 || scope.EntryCount > evidence.AllocationEntryCount {
			return fmt.Errorf("scope %s entry count must be positive and no greater than allocation count",
				scope.ID)
		}
		if _, err := audit.DigestHex(scope.ChainHead); err != nil {
			return fmt.Errorf("scope %s chain head: %w", scope.ID, err)
		}
		if previousScopeID != "" && scope.ID <= previousScopeID {
			return errors.New("audit evidence scopes must be uniquely sorted by ID")
		}
		previousScopeID = scope.ID
	}
	return nil
}

func checkAuditEvidencePrefix(
	ctx context.Context, snapshot *MetadataSnapshot, current, expected AuditEvidence,
) (AuditEvidenceCheck, error) {
	check := AuditEvidenceCheck{Extends: true, Problems: []AuditEvidenceProblem{}}
	add := func(code, scopeID, message string) {
		check.Extends = false
		check.Problems = append(check.Problems, AuditEvidenceProblem{
			Code: code, ScopeID: scopeID, Message: message,
		})
	}
	if !current.Enabled {
		add("audit_not_enabled", "", "current vault has no audit authority")
		return check, nil
	}
	if current.VaultID != expected.VaultID {
		add("vault_mismatch", "", fmt.Sprintf(
			"expected vault %s, current vault is %s", expected.VaultID, current.VaultID,
		))
		return check, nil
	}
	if current.LineageID != expected.LineageID {
		add("lineage_mismatch", "", fmt.Sprintf(
			"expected allocation lineage %s, current lineage is %s",
			expected.LineageID, current.LineageID,
		))
		return check, nil
	}
	if current.AllocationEntryCount < expected.AllocationEntryCount {
		add("allocation_shorter", "", fmt.Sprintf(
			"expected allocation entry %d, current lineage ends at %d",
			expected.AllocationEntryCount, current.AllocationEntryCount,
		))
	} else {
		digest, err := auditRecordDigestAt(ctx, snapshot, "allocation_entry", "", expected.AllocationEntryCount)
		if err != nil {
			return AuditEvidenceCheck{}, err
		}
		if digest != expected.AllocationHead {
			add("allocation_diverged", "", fmt.Sprintf(
				"allocation entry %d has head %s, expected %s",
				expected.AllocationEntryCount, digest, expected.AllocationHead,
			))
		}
	}
	currentScopes := make(map[string]AuditScopeEvidence, len(current.Scopes))
	for _, scope := range current.Scopes {
		currentScopes[scope.ID] = scope
	}
	for _, expectedScope := range expected.Scopes {
		currentScope, ok := currentScopes[expectedScope.ID]
		if !ok {
			add("scope_missing", expectedScope.ID,
				fmt.Sprintf("expected audit scope %s is missing", expectedScope.ID))
			continue
		}
		if currentScope.EntryCount < expectedScope.EntryCount {
			add("scope_shorter", expectedScope.ID, fmt.Sprintf(
				"scope %s expected entry %d, current chain ends at %d",
				expectedScope.ID, expectedScope.EntryCount, currentScope.EntryCount,
			))
			continue
		}
		digest, err := auditRecordDigestAt(
			ctx, snapshot, "scope_chain_entry", expectedScope.ID, expectedScope.EntryCount,
		)
		if err != nil {
			return AuditEvidenceCheck{}, err
		}
		if digest != expectedScope.ChainHead {
			add("scope_diverged", expectedScope.ID, fmt.Sprintf(
				"scope %s entry %d has head %s, expected %s", expectedScope.ID,
				expectedScope.EntryCount, digest, expectedScope.ChainHead,
			))
		}
	}
	return check, nil
}

func auditRecordDigestAt(
	ctx context.Context, snapshot *MetadataSnapshot, kind, scopeID string, count int64,
) (string, error) {
	var digest string
	var err error
	if kind == "allocation_entry" {
		err = snapshot.QueryRowContext(ctx, `SELECT digest FROM audit_records
			WHERE kind='allocation_entry' AND operation_sequence=?`, count).Scan(&digest)
	} else {
		err = snapshot.QueryRowContext(ctx, `SELECT digest FROM audit_records
			WHERE kind='scope_chain_entry' AND scope_id=? AND entry_count=?`,
			scopeID, count).Scan(&digest)
	}
	if err != nil {
		return "", fmt.Errorf("reading verified %s prefix entry %d: %w", kind, count, err)
	}
	return digest, nil
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
