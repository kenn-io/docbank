package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"go.kenn.io/docbank/document"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

const productionEmailInventoryComplete = "complete"

// PinProductionRevisionEvidence retains the source-derived facts and exact
// email publication chosen for a sealed revision. Operation IDs are selectors,
// never a source of facts. An omitted selector is accepted only when the root
// has one publication total, and that publication is complete.
func (s *Store) PinProductionRevisionEvidence(ctx context.Context, setID string, revision int64, emailOperations map[string]string) error {
	if s == nil || validateUUIDv4(setID) != nil || revision < 1 {
		return ErrInvalidProduction
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		stored, err := s.loadProductionInputsTx(ctx, tx, setID, revision)
		if err != nil || !stored.Draft.MembershipSealed || stored.Draft.State != "draft" {
			return errors.Join(ErrInvalidProduction, err)
		}
		if !productionPolicyFieldsSupported(stored.Policy) {
			return ErrInvalidProduction
		}
		var authorityRows int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM production_revision_gate_authority
			WHERE set_id=? AND revision=?`, setID, revision).Scan(&authorityRows); err != nil {
			return err
		}
		authoritySelected := authorityRows != 0
		if authoritySelected {
			if err := checkProductionEvidenceHeadsTx(ctx, tx, stored.Members); err != nil {
				return err
			}
		}
		requiresMetadata := productionPolicyUsesMetadata(stored.Policy)
		selectedRoots := make(map[string]string)
		for _, prepared := range stored.Members {
			member := prepared.Member
			if member.Family.Kind == "email_message" || member.Family.Kind == "email_attachment" {
				root := member.Family.RootVersionID
				operationID, found := selectedRoots[root]
				if !found {
					operationID = emailOperations[root]
					if operationID == "" {
						var count int
						if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM email_document_publications WHERE parent_version_id=?`, root).Scan(&count); err != nil || count != 1 {
							return errors.Join(ErrInvalidProduction, err)
						}
						if err := tx.QueryRowContext(ctx, `SELECT operation_id FROM email_document_publications WHERE parent_version_id=?`, root).Scan(&operationID); err != nil {
							return errors.Join(ErrInvalidProduction, err)
						}
					}
					if document.ValidateEmailDocumentOperationID(operationID) != nil {
						return ErrInvalidProduction
					}
					if err := pinProductionEmailPublicationTx(ctx, tx, setID, revision, root, operationID,
						!authoritySelected); err != nil {
						return err
					}
					selectedRoots[root] = operationID
				}
				if member.Family.Kind == "email_attachment" && member.Family.RelationOperationID != operationID {
					return ErrInvalidProduction
				}
			}
			generation, metadata, err := activeSourceMetadata(ctx, tx, member.SourceSHA256)
			if errors.Is(err, ErrNotFound) && !requiresMetadata {
				continue
			}
			if err != nil {
				return errors.Join(ErrInvalidProduction, err)
			}
			if generation.SourceSHA256 != member.SourceSHA256 ||
				sourceMetadataGenerationID(generation.SourceSHA256, generation.ContractVersion,
					generation.ExtractorFingerprint, generation.Checksum) != generation.GenerationID {
				return ErrInvalidProduction
			}
			facts := productionPolicyMemberFacts(member, prepared.Facts.Labels)
			if _, found := selectedRoots[member.Family.RootVersionID]; found {
				facts.FamilyComplete = true
			}
			productionAddMetadataFacts(&facts, metadata)
			canonicalJSON, digest, err := documentproduction.CanonicalPolicyMemberFacts(facts)
			if err != nil {
				return errors.Join(ErrInvalidProduction, err)
			}
			var existing, existingGeneration, existingEvidence, existingSource string
			err = tx.QueryRowContext(ctx, `SELECT facts_sha256,generation_id,evidence_sha256,source_sha256
				FROM production_member_policy_facts WHERE set_id=? AND revision=? AND member_id=?`,
				setID, revision, member.ID).Scan(&existing, &existingGeneration, &existingEvidence, &existingSource)
			if err == nil {
				if existing != digest || existingGeneration != generation.GenerationID ||
					existingEvidence != generation.Checksum || existingSource != member.SourceSHA256 {
					return ErrInvalidProduction
				}
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if authoritySelected {
				return ErrInvalidProduction
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO production_member_policy_facts
				(set_id,revision,member_id,source_sha256,generation_id,evidence_sha256,allowlist_version,facts_sha256,canonical_json)
				VALUES(?,?,?,?,?,?,?,?,?)`, setID, revision, member.ID, member.SourceSHA256, generation.GenerationID,
				generation.Checksum, documentproduction.PolicyFactsAllowlistV1, digest, canonicalJSON)
			if err != nil {
				return errors.Join(ErrInvalidProduction, err)
			}
		}
		for root := range emailOperations {
			if _, found := selectedRoots[root]; !found {
				return ErrInvalidProduction
			}
		}
		return nil
	})
}

func productionPolicyFieldsSupported(policy documentproduction.PolicyVersion) bool {
	for _, rule := range policy.Rules {
		switch rule.Predicate.Field {
		case "member.id", "family.id", "family.kind", "label", "labels", "metadata.title", "metadata.subject", "document.date":
		default:
			return false
		}
	}
	return true
}

func productionPolicyUsesMetadata(policy documentproduction.PolicyVersion) bool {
	for _, rule := range policy.Rules {
		switch rule.Predicate.Field {
		case "metadata.title", "metadata.subject", "document.date":
			return true
		}
	}
	return false
}

func productionAddMetadataFacts(facts *documentproduction.PolicyMemberFacts, metadata document.SourceMetadataV1) {
	for _, field := range metadata.Fields {
		if field.Sensitive {
			continue
		}
		switch field.Key {
		case "title", "subject":
			if field.Value.Kind == document.SourceMetadataString && field.Value.String != nil && *field.Value.String != "" {
				facts.Fields["metadata."+field.Key] = []string{*field.Value.String}
			}
		case "email.sent":
			stamp := field.Value.Timestamp
			if field.Value.Kind == document.SourceMetadataTimestamp && stamp != nil && len(stamp.Normalized) >= 10 &&
				stamp.Precision != "" {
				date := stamp.Normalized[:10]
				if parsed, err := time.Parse("2006-01-02", date); err == nil && parsed.Format("2006-01-02") == date {
					facts.Fields["document.date"] = []string{date}
				}
			}
		}
	}
}

func pinProductionEmailPublicationTx(ctx context.Context, tx *sql.Tx, setID string, revision int64,
	root, operationID string, allowInsert bool) error {
	record, err := loadEmailDocumentPublication(ctx, tx, operationID)
	if err != nil || record.Request.Parent.VersionID != root || record.Receipt.InventoryState != productionEmailInventoryComplete {
		return errors.Join(ErrInvalidProduction, err)
	}
	if err := validateProductionEmailIdentityTx(ctx, tx, record.Request.Parent); err != nil {
		return err
	}
	if err := validateProductionEmailRelationsTx(ctx, tx, record.Receipt); err != nil {
		return err
	}
	receiptJSON, err := canonical.Marshal(record.Receipt)
	if err != nil {
		return err
	}
	receiptDigest := productionSHA256(receiptJSON)
	var oldOperation, oldRequest, oldReceipt string
	err = tx.QueryRowContext(ctx, `SELECT operation_id,request_digest,receipt_sha256 FROM production_revision_email_publications
		WHERE set_id=? AND revision=? AND root_version_id=?`, setID, revision, root).Scan(&oldOperation, &oldRequest, &oldReceipt)
	if err == nil {
		if oldOperation != operationID || oldRequest != record.Receipt.RequestDigest || oldReceipt != receiptDigest {
			return ErrInvalidProduction
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !allowInsert {
		return ErrInvalidProduction
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO production_revision_email_publications
		(set_id,revision,root_version_id,operation_id,request_digest,receipt_sha256) VALUES(?,?,?,?,?,?)`,
		setID, revision, root, operationID, record.Receipt.RequestDigest, receiptDigest)
	if err != nil {
		return errors.Join(ErrInvalidProduction, err)
	}
	return nil
}

func validateProductionEmailRelationsTx(ctx context.Context, tx *sql.Tx, receipt document.EmailDocumentPublicationReceipt) error {
	rows, err := tx.QueryContext(ctx, `SELECT occurrence_order,child_version_id FROM email_document_relations
		WHERE operation_id=? ORDER BY occurrence_order`, receipt.OperationID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	index := 0
	for rows.Next() {
		var order int
		var child sql.NullString
		if err := rows.Scan(&order, &child); err != nil {
			return errors.Join(ErrInvalidProduction, err, rows.Close())
		}
		if index >= len(receipt.Relations) || receipt.Relations[index].Order != order ||
			(child.Valid != (receipt.Relations[index].Child != nil)) ||
			(child.Valid && child.String != receipt.Relations[index].Child.VersionID) {
			return errors.Join(ErrInvalidProduction, rows.Close())
		}
		if child.Valid {
			if err := validateProductionEmailIdentityTx(ctx, tx, *receipt.Relations[index].Child); err != nil {
				return errors.Join(err, rows.Close())
			}
		}
		index++
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil || index != len(receipt.Relations) {
		return errors.Join(ErrInvalidProduction, err)
	}
	return nil
}

func validateProductionEmailIdentityTx(ctx context.Context, tx *sql.Tx, identity document.EmailDocumentIdentity) error {
	var nodeID, size int64
	var sourceSHA256 string
	err := tx.QueryRowContext(ctx, `SELECT node_id,blob_hash,size FROM content_versions WHERE version_id=?`,
		identity.VersionID).Scan(&nodeID, &sourceSHA256, &size)
	if err != nil || nodeID != identity.NodeID || sourceSHA256 != identity.SHA256 || size != identity.Size {
		return errors.Join(ErrInvalidProduction, err)
	}
	return nil
}

func loadProductionEvidencePinTx(ctx context.Context, tx *sql.Tx, setID string, revision int64,
	member redaction.Member, facts documentproduction.PolicyMemberFacts) (*documentproduction.ProductionMemberEvidencePin, documentproduction.PolicyMemberFacts, bool, error) {
	pin := &documentproduction.ProductionMemberEvidencePin{}
	var sourceSHA256, generationID, evidenceSHA256, allowlistVersion, factsSHA256 string
	var factsJSON []byte
	err := tx.QueryRowContext(ctx, `SELECT source_sha256,generation_id,evidence_sha256,allowlist_version,facts_sha256,canonical_json
		FROM production_member_policy_facts WHERE set_id=? AND revision=? AND member_id=?`, setID, revision, member.ID).
		Scan(&sourceSHA256, &generationID, &evidenceSHA256, &allowlistVersion, &factsSHA256, &factsJSON)
	hasFacts := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, facts, false, err
	}
	if hasFacts {
		var raw []byte
		var storedSource, contract, fingerprint, storedChecksum string
		err = tx.QueryRowContext(ctx, `SELECT source_sha256,contract_version,extractor_fingerprint,canonical_json,checksum
			FROM source_metadata_generations WHERE generation_id=?`, generationID).
			Scan(&storedSource, &contract, &fingerprint, &raw, &storedChecksum)
		if err != nil || sourceSHA256 != member.SourceSHA256 || storedSource != sourceSHA256 ||
			storedChecksum != evidenceSHA256 || allowlistVersion != documentproduction.PolicyFactsAllowlistV1 {
			return nil, facts, false, errors.Join(ErrInvalidProduction, err)
		}
		metadata, checksum, err := document.DecodeSourceMetadataV1(raw)
		if err != nil || checksum != evidenceSHA256 || contract != metadata.ContractVersion ||
			sourceMetadataGenerationID(storedSource, contract, fingerprint, checksum) != generationID {
			return nil, facts, false, errors.Join(ErrInvalidProduction, err)
		}
		productionAddMetadataFacts(&facts, metadata)
		pin.AllowlistVersion, pin.SourceMetadataGenerationID = allowlistVersion, generationID
		pin.SourceMetadataEvidenceSHA256, pin.PolicyFactsSHA256 = evidenceSHA256, factsSHA256
	}
	var operationID, requestDigest, receiptDigest string
	err = tx.QueryRowContext(ctx, `SELECT operation_id,request_digest,receipt_sha256 FROM production_revision_email_publications
		WHERE set_id=? AND revision=? AND root_version_id=?`, setID, revision, member.Family.RootVersionID).
		Scan(&operationID, &requestDigest, &receiptDigest)
	hasEmail := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, facts, false, err
	}
	if hasEmail {
		if member.Family.Kind != "email_message" && member.Family.Kind != "email_attachment" {
			return nil, facts, false, ErrInvalidProduction
		}
		record, err := loadEmailDocumentPublication(ctx, tx, operationID)
		if err != nil || record.Request.Parent.VersionID != member.Family.RootVersionID ||
			record.Receipt.InventoryState != productionEmailInventoryComplete || record.Receipt.RequestDigest != requestDigest {
			return nil, facts, false, errors.Join(ErrInvalidProduction, err)
		}
		if err := validateProductionEmailRelationsTx(ctx, tx, record.Receipt); err != nil {
			return nil, facts, false, err
		}
		if err := validateProductionEmailIdentityTx(ctx, tx, record.Request.Parent); err != nil {
			return nil, facts, false, err
		}
		canonicalJSON, err := canonical.Marshal(record.Receipt)
		if err != nil || productionSHA256(canonicalJSON) != receiptDigest {
			return nil, facts, false, errors.Join(ErrInvalidProduction, err)
		}
		if member.Family.Kind == "email_attachment" {
			if member.Family.RelationOperationID != operationID || member.Family.RelationOrder < 1 ||
				member.Family.RelationOrder > len(record.Receipt.Relations) ||
				record.Receipt.Relations[member.Family.RelationOrder-1].Child == nil ||
				record.Receipt.Relations[member.Family.RelationOrder-1].Child.VersionID != member.SourceVersionID {
				return nil, facts, false, ErrInvalidProduction
			}
		}
		pin.EmailRootVersionID, pin.EmailPublicationOperationID = member.Family.RootVersionID, operationID
		pin.EmailPublicationRequestSHA256, pin.EmailPublicationReceiptSHA256 = requestDigest, receiptDigest
		facts.FamilyComplete = true
	}
	if hasFacts {
		canonicalJSON, digest, err := documentproduction.CanonicalPolicyMemberFacts(facts)
		if err != nil || digest != factsSHA256 || !bytes.Equal(canonicalJSON, factsJSON) {
			return nil, facts, false, errors.Join(ErrInvalidProduction, err)
		}
	}
	if !hasFacts && !hasEmail {
		return pin, facts, false, nil
	}
	return pin, facts, true, nil
}

func checkProductionEvidenceHeadsTx(ctx context.Context, tx *sql.Tx, members []productionservice.StoredPreparedMember) error {
	for _, member := range members {
		if member.EvidencePin == nil || member.EvidencePin.SourceMetadataGenerationID == "" {
			continue
		}
		var current string
		err := tx.QueryRowContext(ctx, `SELECT generation_id FROM source_metadata_heads WHERE source_sha256=?`,
			member.Member.SourceSHA256).Scan(&current)
		if err != nil || current != member.EvidencePin.SourceMetadataGenerationID {
			return errors.Join(ErrInvalidProduction, err)
		}
	}
	return nil
}

func productionEvidenceAllPinned(members []productionservice.StoredPreparedMember) bool {
	return !slices.ContainsFunc(members, func(member productionservice.StoredPreparedMember) bool {
		return member.EvidencePin == nil || member.EvidencePin.PolicyFactsSHA256 == ""
	})
}

func productionRequiredEvidencePinned(policy documentproduction.PolicyVersion,
	members []productionservice.StoredPreparedMember) bool {
	return (!productionPolicyUsesMetadata(policy) || productionEvidenceAllPinned(members)) &&
		!slices.ContainsFunc(members, func(member productionservice.StoredPreparedMember) bool {
			return (member.Member.Family.Kind == "email_message" || member.Member.Family.Kind == "email_attachment") &&
				(member.EvidencePin == nil || member.EvidencePin.EmailPublicationOperationID == "")
		})
}

// ProductionRevisionGateSelection names authority; its digests and contents
// are always derived from stored grants and frozen receipts.
type ProductionRevisionGateSelection struct {
	ApprovalID           string
	PrivilegeLogID       string
	PrivilegeLogRevision int64
}

func (s *Store) SelectProductionRevisionGateAuthority(ctx context.Context, setID string, revision int64,
	selection ProductionRevisionGateSelection) error {
	if s == nil || validateUUIDv4(setID) != nil || revision < 1 ||
		selection.ApprovalID != "" && validateUUIDv4(selection.ApprovalID) != nil ||
		(selection.PrivilegeLogID == "") != (selection.PrivilegeLogRevision == 0) ||
		selection.PrivilegeLogID != "" && (validateUUIDv4(selection.PrivilegeLogID) != nil || selection.PrivilegeLogRevision < 1) {
		return ErrInvalidProduction
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		stored, err := s.loadProductionInputsTx(ctx, tx, setID, revision)
		if err != nil || !stored.Draft.MembershipSealed || stored.Draft.State != "draft" || !productionPolicyFieldsSupported(stored.Policy) ||
			!productionRequiredEvidencePinned(stored.Policy, stored.Members) {
			return errors.Join(ErrInvalidProduction, err)
		}
		if err := checkProductionEvidenceHeadsTx(ctx, tx, stored.Members); err != nil {
			return err
		}
		approvalRequired := stored.Policy.Approval.Required
		privilegeRequired := stored.Policy.PrivilegeLog.Required || stored.Policy.PrivilegeLog.RequireFrozenReceipt
		if approvalRequired != (selection.ApprovalID != "") || privilegeRequired != (selection.PrivilegeLogID != "") {
			return ErrInvalidProduction
		}
		var approvalDigest, subjectDigest, privilegeDigest string
		if selection.PrivilegeLogID != "" {
			receipt, err := loadProductionSelectedPrivilegeTx(ctx, tx, stored, selection.PrivilegeLogID,
				selection.PrivilegeLogRevision)
			if err != nil {
				return err
			}
			stored.PrivilegeLog = &receipt
			privilegeDigest = receipt.SHA256
		}
		if selection.ApprovalID != "" {
			grant, subject, _, err := loadProductionApproval(ctx, tx, selection.ApprovalID)
			if err != nil {
				return errors.Join(ErrInvalidProduction, err)
			}
			expected, err := productionservice.StoredApprovalSubject(stored)
			if err != nil {
				return errors.Join(ErrInvalidProduction, err)
			}
			_, expectedDigest, err := documentproduction.CanonicalApprovalSubject(expected)
			if err != nil || grant.SubjectSHA256 != expectedDigest {
				return errors.Join(ErrInvalidProduction, err)
			}
			_, actualDigest, err := documentproduction.CanonicalApprovalSubject(subject)
			if err != nil || actualDigest != expectedDigest {
				return errors.Join(ErrInvalidProduction, err)
			}
			approvalDigest, subjectDigest = grant.SHA256, expectedDigest
		}
		if err := validateProductionRevisionGateAuthority(productionRevisionGateAuthority{
			SetID: setID, Revision: revision,
			ApprovalID: selection.ApprovalID, ApprovalGrantSHA256: approvalDigest,
			ApprovalSubjectSHA256: subjectDigest, PrivilegeLogID: selection.PrivilegeLogID,
			PrivilegeLogRevision: selection.PrivilegeLogRevision, PrivilegeLogReceiptSHA256: privilegeDigest,
		}); err != nil {
			return err
		}
		var oldApprovalID, oldApprovalDigest, oldSubjectDigest sql.NullString
		var oldLogID, oldLogDigest sql.NullString
		var oldLogRevision sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT approval_id,approval_grant_sha256,approval_subject_sha256,
			privilege_log_id,privilege_log_revision,privilege_log_receipt_sha256
			FROM production_revision_gate_authority WHERE set_id=? AND revision=?`, setID, revision).
			Scan(&oldApprovalID, &oldApprovalDigest, &oldSubjectDigest, &oldLogID, &oldLogRevision, &oldLogDigest)
		if err == nil {
			if oldApprovalID.String != selection.ApprovalID || oldApprovalDigest.String != approvalDigest ||
				oldSubjectDigest.String != subjectDigest || oldLogID.String != selection.PrivilegeLogID ||
				oldLogRevision.Int64 != selection.PrivilegeLogRevision || oldLogDigest.String != privilegeDigest {
				return ErrInvalidProduction
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO production_revision_gate_authority
			(set_id,revision,approval_id,approval_grant_sha256,approval_subject_sha256,
			privilege_log_id,privilege_log_revision,privilege_log_receipt_sha256) VALUES(?,?,?,?,?,?,?,?)`,
			setID, revision, nullableProductionValue(selection.ApprovalID), nullableProductionValue(approvalDigest),
			nullableProductionValue(subjectDigest), nullableProductionValue(selection.PrivilegeLogID),
			nullableProductionRevision(selection.PrivilegeLogRevision), nullableProductionValue(privilegeDigest))
		if err != nil {
			return errors.Join(ErrInvalidProduction, err)
		}
		return nil
	})
}

func nullableProductionValue(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableProductionRevision(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func loadProductionSelectedPrivilegeTx(ctx context.Context, tx *sql.Tx, stored productionservice.StoredProductionInputs,
	logID string, revision int64) (documentproduction.PrivilegeLogReceipt, error) {
	var raw []byte
	var digest string
	err := tx.QueryRowContext(ctx, `SELECT canonical_json,sha256 FROM production_privilege_log_receipts
		WHERE log_id=? AND revision=?`, logID, revision).Scan(&raw, &digest)
	if err != nil {
		return documentproduction.PrivilegeLogReceipt{}, errors.Join(ErrInvalidProduction, err)
	}
	receipt, err := decodePrivilegeReceipt(raw, digest)
	if err != nil || receipt.LogID != logID || receipt.Revision != revision ||
		receipt.PolicySHA256 != stored.Policy.SHA256 || stored.Withheld == nil ||
		receipt.WithheldSelectionSHA256 != stored.Withheld.SHA256 {
		return documentproduction.PrivilegeLogReceipt{}, errors.Join(ErrInvalidProduction, err)
	}
	log, err := loadStoredPrivilegeLog(ctx, tx, logID, revision)
	if err != nil || log.Validation == nil || log.Validation.DraftGeneration != log.Generation ||
		log.Validation.Validation.InputsSHA256 != receipt.InputsSHA256 ||
		log.Validation.Validation.RowsSHA256 != receipt.RowsSHA256 ||
		log.Policy.SHA256 != stored.Policy.SHA256 || log.Withheld.SHA256 != stored.Withheld.SHA256 ||
		log.Players.SHA256 != receipt.PlayersSHA256 {
		return documentproduction.PrivilegeLogReceipt{}, errors.Join(ErrInvalidProduction, err)
	}
	validatedAt, err := time.Parse(time.RFC3339Nano, receipt.ValidatedAt)
	if err != nil {
		return documentproduction.PrivilegeLogReceipt{}, errors.Join(ErrInvalidProduction, err)
	}
	validation, err := documentproduction.ValidatePrivilegeLog(documentproduction.PrivilegeLogValidationInput{
		LogID: logID, Revision: revision, WithheldSelectionSHA256: log.Withheld.SHA256,
		PolicySHA256: log.Policy.SHA256, PlayersSHA256: log.Players.SHA256,
		ValidatedAt: validatedAt.Format(time.RFC3339Nano), Rows: log.Rows,
	}, log.Withheld, log.Policy, log.Players, log.Produced)
	if err != nil || validation.InputsSHA256 != receipt.InputsSHA256 || validation.RowsSHA256 != receipt.RowsSHA256 {
		return documentproduction.PrivilegeLogReceipt{}, errors.Join(ErrInvalidProduction, err)
	}
	if receipt.ApprovalEvaluationSHA256 != "" {
		if log.ApprovalEvaluation == nil || log.ApprovalEvaluation.SHA256 != receipt.ApprovalEvaluationSHA256 {
			return documentproduction.PrivilegeLogReceipt{}, ErrInvalidProduction
		}
	}
	return receipt, nil
}

func loadProductionGateAuthorityTx(ctx context.Context, tx *sql.Tx, at time.Time,
	stored *productionservice.StoredProductionInputs) error {
	var approvalID, approvalDigest, subjectDigest sql.NullString
	var logID, logDigest sql.NullString
	var logRevision sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT approval_id,approval_grant_sha256,approval_subject_sha256,
		privilege_log_id,privilege_log_revision,privilege_log_receipt_sha256
		FROM production_revision_gate_authority WHERE set_id=? AND revision=?`, stored.Draft.SetID, stored.Draft.Revision).
		Scan(&approvalID, &approvalDigest, &subjectDigest, &logID, &logRevision, &logDigest)
	if errors.Is(err, sql.ErrNoRows) {
		if stored.Policy.Approval.Required || stored.Policy.PrivilegeLog.Required || stored.Policy.PrivilegeLog.RequireFrozenReceipt {
			return ErrInvalidProduction
		}
		return nil
	}
	if err != nil || approvalID.Valid != approvalDigest.Valid || approvalID.Valid != subjectDigest.Valid ||
		logID.Valid != logRevision.Valid || logID.Valid != logDigest.Valid ||
		stored.Policy.Approval.Required != approvalID.Valid ||
		(stored.Policy.PrivilegeLog.Required || stored.Policy.PrivilegeLog.RequireFrozenReceipt) != logID.Valid {
		return errors.Join(ErrInvalidProduction, err)
	}
	if err := validateProductionRevisionGateAuthority(productionRevisionGateAuthority{
		SetID: stored.Draft.SetID, Revision: stored.Draft.Revision,
		ApprovalID: approvalID.String, ApprovalGrantSHA256: approvalDigest.String,
		ApprovalSubjectSHA256: subjectDigest.String, PrivilegeLogID: logID.String,
		PrivilegeLogRevision: logRevision.Int64, PrivilegeLogReceiptSHA256: logDigest.String,
	}); err != nil {
		return err
	}
	if logID.Valid {
		receipt, err := loadProductionSelectedPrivilegeTx(ctx, tx, *stored, logID.String, logRevision.Int64)
		if err != nil || receipt.SHA256 != logDigest.String {
			return errors.Join(ErrInvalidProduction, err)
		}
		stored.PrivilegeLog = &receipt
	}
	if approvalID.Valid {
		grant, subject, _, err := loadProductionApproval(ctx, tx, approvalID.String)
		if err != nil || grant.SHA256 != approvalDigest.String || grant.SubjectSHA256 != subjectDigest.String {
			return errors.Join(ErrInvalidProduction, err)
		}
		expected, err := productionservice.StoredApprovalSubject(*stored)
		if err != nil {
			return errors.Join(ErrInvalidProduction, err)
		}
		_, expectedDigest, err := documentproduction.CanonicalApprovalSubject(expected)
		if err != nil || expectedDigest != subjectDigest.String {
			return errors.Join(ErrInvalidProduction, err)
		}
		_, actualDigest, err := documentproduction.CanonicalApprovalSubject(subject)
		if err != nil || actualDigest != expectedDigest {
			return errors.Join(ErrInvalidProduction, err)
		}
		events, err := loadProductionApprovalEvents(ctx, tx, approvalID.String)
		if err != nil {
			return errors.Join(ErrInvalidProduction, err)
		}
		evaluation, err := productionservice.EvaluateRequiredApproval(stored.Policy, expected, &grant, events, at)
		if err != nil || evaluation.State != documentproduction.ApprovalStateCurrent {
			return errors.Join(ErrInvalidProduction, err)
		}
		stored.ApprovalSubject, stored.ApprovalGrant = &subject, &grant
		stored.ApprovalEvents, stored.ApprovalEvaluation = events, &evaluation
	}
	return nil
}
