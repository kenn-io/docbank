package store

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/canonical"
)

// productionRevisionGateAuthority is the exact optional approval and
// privilege authority selected by one immutable production revision.
type productionRevisionGateAuthority struct {
	SetID                     string
	Revision                  int64
	ApprovalID                string
	ApprovalGrantSHA256       string
	ApprovalSubjectSHA256     string
	PrivilegeLogID            string
	PrivilegeLogRevision      int64
	PrivilegeLogReceiptSHA256 string
}

func validateProductionRevisionGateAuthority(value productionRevisionGateAuthority) error {
	if validateUUIDv4(value.SetID) != nil || value.Revision < 1 {
		return ErrInvalidProduction
	}
	approvalCount := 0
	for _, item := range []string{value.ApprovalID, value.ApprovalGrantSHA256, value.ApprovalSubjectSHA256} {
		if item != "" {
			approvalCount++
		}
	}
	if approvalCount != 0 && approvalCount != 3 || approvalCount == 3 &&
		(validateUUIDv4(value.ApprovalID) != nil ||
			!canonical.IsSHA256Hex(value.ApprovalGrantSHA256) || !canonical.IsSHA256Hex(value.ApprovalSubjectSHA256)) {
		return ErrInvalidProduction
	}
	privilegePresent := value.PrivilegeLogID != "" || value.PrivilegeLogRevision != 0 || value.PrivilegeLogReceiptSHA256 != ""
	if privilegePresent && (validateUUIDv4(value.PrivilegeLogID) != nil || value.PrivilegeLogRevision < 1 ||
		!canonical.IsSHA256Hex(value.PrivilegeLogReceiptSHA256)) {
		return ErrInvalidProduction
	}
	return nil
}

func validateProductionGateAuthorityState(ctx context.Context, q metadataQuerier) error {
	rows, err := q.QueryContext(ctx, `SELECT p.set_id,p.revision,COALESCE(p.approval_id,''),
		COALESCE(p.approval_grant_sha256,''),COALESCE(p.approval_subject_sha256,''),
		COALESCE(p.privilege_log_id,''),COALESCE(p.privilege_log_revision,0),
		COALESCE(p.privilege_log_receipt_sha256,''),COALESCE(g.subject_sha256,'')
		FROM production_revision_gate_authority p LEFT JOIN production_approval_grants g
		ON g.approval_id=p.approval_id AND g.grant_sha256=p.approval_grant_sha256
		ORDER BY p.set_id,p.revision`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var value productionRevisionGateAuthority
		var grantSubjectSHA256 string
		if err := rows.Scan(&value.SetID, &value.Revision, &value.ApprovalID,
			&value.ApprovalGrantSHA256, &value.ApprovalSubjectSHA256,
			&value.PrivilegeLogID, &value.PrivilegeLogRevision,
			&value.PrivilegeLogReceiptSHA256, &grantSubjectSHA256); err != nil {
			return err
		}
		if err := validateProductionRevisionGateAuthority(value); err != nil {
			return err
		}
		if value.ApprovalID != "" && value.ApprovalSubjectSHA256 != grantSubjectSHA256 {
			return ErrInvalidProduction
		}
	}
	iterationErr := rows.Err()
	closeErr := rows.Close()
	return errors.Join(iterationErr, closeErr)
}
