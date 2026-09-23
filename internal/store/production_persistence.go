package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

const (
	productionOperationPolicy        = "policy_version"
	productionOperationApproval      = "approval_grant"
	productionOperationApprovalEvent = "approval_event"
	productionOperationPlayers       = "players_snapshot"
	productionOperationWithheld      = "withheld_selection"
	productionOperationDraft         = "privilege_log_draft"
	productionOperationRows          = "privilege_log_rows"
	productionOperationValidation    = "privilege_log_validation"
	productionOperationApprovalBind  = "privilege_log_approval"
	productionOperationFreeze        = "privilege_log_freeze"
	productionOperationAttachment    = "privilege_log_attachment"
)

// PrivilegeLogDraftAuthority supplies the stored inputs not carried by the
// service's prepared draft identity. Rows remain private storage authority.
type PrivilegeLogDraftAuthority struct {
	Draft         productionservice.PreparedPrivilegeLogDraft
	PlayersSHA256 string
	Produced      []redaction.Member
	Rows          []documentproduction.PrivilegeRow
}

// PrivilegeLogRowUpdate is an optimistic, replay-safe replacement of draft
// rows. A successful replacement advances Generation and invalidates prior
// validation and approval bindings.
type PrivilegeLogRowUpdate struct {
	OperationID        string
	LogID              string
	Revision           int64
	ExpectedGeneration int64
	Rows               []documentproduction.PrivilegeRow
}

// PrivilegeLogApprovalBinding pins the evaluation admitted for a validated
// draft. Freeze still reloads the grant and all events and re-evaluates them.
type PrivilegeLogApprovalBinding struct {
	OperationID        string
	LogID              string
	Revision           int64
	ExpectedGeneration int64
	ApprovalID         string
	Evaluation         documentproduction.ApprovalEvaluation
}

type productionGenerationReceipt struct {
	Generation int64 `json:"generation"`
}

func (s *Store) PutProductionPolicy(ctx context.Context, prepared productionservice.PreparedPolicyVersion) (documentproduction.PolicyVersion, error) {
	var result documentproduction.PolicyVersion
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if replay, ok, err := productionOperationReplay[documentproduction.PolicyVersion](ctx, tx,
			prepared.OperationID, productionOperationPolicy, prepared.RequestSHA256); err != nil || ok {
			result = replay
			return err
		}
		canonicalJSON, digest, err := documentproduction.CanonicalPolicyVersion(prepared.Policy)
		if err != nil || !validPreparedOperation(prepared.OperationID, prepared.RequestSHA256) ||
			digest != prepared.PolicySHA256 || digest != prepared.RequestSHA256 ||
			prepared.Policy.SHA256 != digest || !bytes.Equal(canonicalJSON, prepared.Canonical) {
			return invalidProductionStorage("invalid prepared production policy")
		}
		var existingDigest string
		err = tx.QueryRowContext(ctx, `SELECT sha256 FROM production_policy_versions
			WHERE policy_id=? AND version=?`, prepared.Policy.ID, prepared.Policy.Version).Scan(&existingDigest)
		if err == nil && existingDigest != digest {
			return changedProductionPayload(prepared.Policy.ID)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, err = tx.ExecContext(ctx, `INSERT INTO production_policy_versions
				(policy_id,version,sha256,canonical_json) VALUES(?,?,?,?)`,
				prepared.Policy.ID, prepared.Policy.Version, digest, canonicalJSON); err != nil {
				return err
			}
		}
		result = prepared.Policy
		return recordProductionOperation(ctx, tx, prepared.OperationID, productionOperationPolicy,
			prepared.RequestSHA256, result)
	})
	return result, err
}

func (s *Store) ProductionPolicy(ctx context.Context, id string, version int64) (documentproduction.PolicyVersion, error) {
	var raw []byte
	var digest string
	err := s.db.QueryRowContext(ctx, `SELECT canonical_json,sha256 FROM production_policy_versions
		WHERE policy_id=? AND version=?`, id, version).Scan(&raw, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return documentproduction.PolicyVersion{}, ErrNotFound
	}
	if err != nil {
		return documentproduction.PolicyVersion{}, err
	}
	return decodeProductionPolicy(raw, digest)
}

func (s *Store) PutProductionPlayersSnapshot(ctx context.Context, prepared productionservice.PreparedPlayersSnapshot) (documentproduction.PlayersSnapshot, error) {
	var result documentproduction.PlayersSnapshot
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if replay, ok, err := productionOperationReplay[documentproduction.PlayersSnapshot](ctx, tx,
			prepared.OperationID, productionOperationPlayers, prepared.RequestSHA256); err != nil || ok {
			result = replay
			return err
		}
		canonicalJSON, digest, err := documentproduction.CanonicalPlayersSnapshot(prepared.Snapshot)
		if err != nil || !validPreparedOperation(prepared.OperationID, prepared.RequestSHA256) ||
			digest != prepared.SnapshotSHA256 || digest != prepared.RequestSHA256 ||
			prepared.Snapshot.SHA256 != digest || !bytes.Equal(canonicalJSON, prepared.Canonical) {
			return invalidProductionStorage("invalid prepared players snapshot")
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT sha256 FROM production_players_snapshots
			WHERE snapshot_id=? AND revision=?`, prepared.Snapshot.ID, prepared.Snapshot.Revision).Scan(&existing)
		if err == nil && existing != digest {
			return changedProductionPayload(prepared.Snapshot.ID)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, err = tx.ExecContext(ctx, `INSERT INTO production_players_snapshots
				(snapshot_id,revision,sha256,canonical_json) VALUES(?,?,?,?)`,
				prepared.Snapshot.ID, prepared.Snapshot.Revision, digest, canonicalJSON); err != nil {
				return err
			}
		}
		result = prepared.Snapshot
		return recordProductionOperation(ctx, tx, prepared.OperationID, productionOperationPlayers,
			prepared.RequestSHA256, result)
	})
	return result, err
}

func (s *Store) PutProductionWithheldSelection(ctx context.Context, prepared productionservice.PreparedWithheldSelection) (documentproduction.WithheldSelection, error) {
	var result documentproduction.WithheldSelection
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if replay, ok, err := productionOperationReplay[documentproduction.WithheldSelection](ctx, tx,
			prepared.OperationID, productionOperationWithheld, prepared.RequestSHA256); err != nil || ok {
			result = replay
			return err
		}
		canonicalJSON, digest, err := documentproduction.CanonicalWithheldSelection(prepared.Selection)
		if err != nil || !validPreparedOperation(prepared.OperationID, prepared.RequestSHA256) ||
			digest != prepared.SelectionSHA256 || digest != prepared.RequestSHA256 ||
			prepared.Selection.SHA256 != digest || !bytes.Equal(canonicalJSON, prepared.Canonical) {
			return invalidProductionStorage("invalid prepared withheld selection")
		}
		if err = requireProductionDigest(ctx, tx, "production_policy_versions", "sha256", prepared.Selection.PolicySHA256); err != nil {
			return err
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT sha256 FROM production_withheld_selections
			WHERE selection_id=?`, prepared.Selection.ID).Scan(&existing)
		if err == nil && existing != digest {
			return changedProductionPayload(prepared.Selection.ID)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, err = tx.ExecContext(ctx, `INSERT INTO production_withheld_selections
				(selection_id,set_id,revision,policy_sha256,sha256,canonical_json) VALUES(?,?,?,?,?,?)`,
				prepared.Selection.ID, prepared.Selection.SetID, prepared.Selection.Revision,
				prepared.Selection.PolicySHA256, digest, canonicalJSON); err != nil {
				return err
			}
		}
		result = prepared.Selection
		return recordProductionOperation(ctx, tx, prepared.OperationID, productionOperationWithheld,
			prepared.RequestSHA256, result)
	})
	return result, err
}

func (s *Store) PutProductionApproval(ctx context.Context, record productionservice.ApprovalRecord) (documentproduction.ApprovalGrant, error) {
	var result documentproduction.ApprovalGrant
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if replay, ok, err := productionOperationReplay[documentproduction.ApprovalGrant](ctx, tx,
			record.OperationID, productionOperationApproval, record.RequestSHA256); err != nil || ok {
			result = replay
			return err
		}
		subjectJSON, subjectDigest, err := documentproduction.CanonicalApprovalSubject(record.Subject)
		if err != nil {
			return err
		}
		authorityJSON, authorityDigest, err := documentproduction.CanonicalApprovalAuthority(record.Authority)
		if err != nil {
			return err
		}
		grantJSON, grantDigest, err := documentproduction.CanonicalApprovalGrant(record.Grant)
		if err != nil || !validPreparedOperation(record.OperationID, record.RequestSHA256) ||
			subjectDigest != record.SubjectSHA256 || !bytes.Equal(subjectJSON, record.CanonicalSubject) ||
			authorityDigest != record.AuthoritySHA256 || record.Grant.AuthoritySHA256 != authorityDigest ||
			record.Grant.SubjectSHA256 != subjectDigest || record.Grant.SHA256 != grantDigest {
			return invalidProductionStorage("invalid prepared production approval")
		}
		expectedRequest, err := digestProductionValue(struct {
			ApprovalID    string `json:"approval_id"`
			SubjectSHA256 string `json:"subject_sha256"`
			Evidence      string `json:"evidence,omitzero"`
		}{record.Grant.ID, subjectDigest, record.Grant.Evidence})
		if err != nil || expectedRequest != record.RequestSHA256 {
			return invalidProductionStorage("invalid production approval request digest")
		}
		policy, err := loadProductionPolicyByDigest(ctx, tx, record.Subject.Policy.PolicySHA256)
		if err != nil {
			return err
		}
		if record.Subject.Policy.PolicyID != policy.ID || record.Subject.Policy.Version != policy.Version {
			return invalidProductionStorage("production approval selects another policy")
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT grant_sha256 FROM production_approval_grants
			WHERE approval_id=?`, record.Grant.ID).Scan(&existing)
		if err == nil && existing != grantDigest {
			return changedProductionPayload(record.Grant.ID)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, err = tx.ExecContext(ctx, `INSERT INTO production_approval_grants
				(approval_id,subject_sha256,subject_json,authority_sha256,authority_json,grant_sha256,grant_json)
				VALUES(?,?,?,?,?,?,?)`, record.Grant.ID, subjectDigest, subjectJSON,
				authorityDigest, authorityJSON, grantDigest, grantJSON); err != nil {
				return err
			}
		}
		result = record.Grant
		return recordProductionOperation(ctx, tx, record.OperationID, productionOperationApproval,
			record.RequestSHA256, result)
	})
	return result, err
}

func (s *Store) PutProductionApprovalEvent(ctx context.Context, record productionservice.ApprovalEventRecord) (documentproduction.ApprovalEvent, error) {
	var result documentproduction.ApprovalEvent
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if replay, ok, err := productionOperationReplay[documentproduction.ApprovalEvent](ctx, tx,
			record.OperationID, productionOperationApprovalEvent, record.RequestSHA256); err != nil || ok {
			result = replay
			return err
		}
		grant, _, _, err := loadProductionApproval(ctx, tx, record.Event.ApprovalID)
		if err != nil {
			return err
		}
		if _, _, err = documentproduction.CanonicalApprovalEvents([]documentproduction.ApprovalEvent{record.Event}); err != nil {
			return err
		}
		verified, err := productionservice.PrepareApprovalEvent(grant, productionservice.ApprovalEventRequest{
			OperationID: record.OperationID, EventID: record.Event.ID, Kind: record.Event.Kind,
			EffectiveAt: record.Event.EffectiveAt, ReplacementApprovalID: record.Event.ReplacementApprovalID,
			Reason: record.Event.Reason,
		})
		if err != nil || verified.RequestSHA256 != record.RequestSHA256 || verified.Event != record.Event {
			return invalidProductionStorage("invalid prepared approval event")
		}
		eventJSON, err := canonical.Marshal(record.Event)
		if err != nil {
			return err
		}
		eventDigest := digestProductionBytes(eventJSON)
		expectedRequest, err := digestProductionValue(struct {
			EventID               string `json:"event_id"`
			ApprovalSHA256        string `json:"approval_sha256"`
			Kind                  string `json:"kind"`
			EffectiveAt           string `json:"effective_at"`
			ReplacementApprovalID string `json:"replacement_approval_id,omitzero"`
			Reason                string `json:"reason,omitzero"`
		}{record.Event.ID, grant.SHA256, record.Event.Kind, record.Event.EffectiveAt,
			record.Event.ReplacementApprovalID, record.Event.Reason})
		if err != nil || !validPreparedOperation(record.OperationID, record.RequestSHA256) ||
			expectedRequest != record.RequestSHA256 {
			return invalidProductionStorage("invalid prepared approval event")
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT event_sha256 FROM production_approval_events
			WHERE event_id=?`, record.Event.ID).Scan(&existing)
		if err == nil && existing != eventDigest {
			return changedProductionPayload(record.Event.ID)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, err = tx.ExecContext(ctx, `INSERT INTO production_approval_events
				(event_id,approval_id,event_sha256,canonical_json) VALUES(?,?,?,?)`,
				record.Event.ID, record.Event.ApprovalID, eventDigest, eventJSON); err != nil {
				return err
			}
		}
		result = record.Event
		return recordProductionOperation(ctx, tx, record.OperationID, productionOperationApprovalEvent,
			record.RequestSHA256, result)
	})
	return result, err
}

func (s *Store) CreatePrivilegeLogDraft(ctx context.Context, authority PrivilegeLogDraftAuthority) (int64, error) {
	var result productionGenerationReceipt
	prepared := authority.Draft
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if replay, ok, err := productionOperationReplay[productionGenerationReceipt](ctx, tx,
			prepared.OperationID, productionOperationDraft, prepared.RequestSHA256); err != nil || ok {
			result = replay
			return err
		}
		if err := validatePreparedPrivilegeDraft(prepared); err != nil {
			return err
		}
		policy, err := loadProductionPolicyByDigest(ctx, tx, prepared.PolicySHA256)
		if err != nil {
			return err
		}
		withheld, err := loadProductionWithheld(ctx, tx, prepared.WithheldSelectionSHA256)
		if err != nil {
			return err
		}
		players, err := loadProductionPlayers(ctx, tx, authority.PlayersSHA256)
		if err != nil {
			return err
		}
		if withheld.PolicySHA256 != policy.SHA256 || documentproduction.ValidateSelectionPartition(authority.Produced, withheld) != nil {
			return invalidProductionStorage("privilege draft authority is inconsistent")
		}
		for _, member := range authority.Produced {
			if err := redaction.ValidateMember(member); err != nil {
				return invalidProductionStorage("invalid produced privilege member")
			}
		}
		producedJSON, err := canonical.Marshal(nonNilProduced(authority.Produced))
		if err != nil {
			return err
		}
		rows, rowsDigest, err := normalizePrivilegeRows(authority.Rows)
		if err != nil {
			return err
		}
		if prepared.PredecessorLogID != "" {
			var predecessorID string
			if err = tx.QueryRowContext(ctx, `SELECT log_id FROM production_privilege_log_receipts
				WHERE sha256=?`, prepared.PredecessorReceiptSHA256).Scan(&predecessorID); err != nil {
				return productionRelationError("privilege predecessor receipt", err)
			}
			if predecessorID != prepared.PredecessorLogID {
				return invalidProductionStorage("privilege predecessor receipt belongs to another log")
			}
		}
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM production_privilege_log_drafts WHERE log_id=? AND revision=?)`,
			prepared.LogID, prepared.Revision).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return changedProductionPayload(prepared.LogID)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO production_privilege_log_drafts
			(log_id,revision,generation,predecessor_log_id,predecessor_receipt_sha256,
			 withheld_selection_sha256,policy_sha256,players_sha256,produced_sha256,produced_json,rows_sha256)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, prepared.LogID, prepared.Revision, 1,
			nullableProductionText(prepared.PredecessorLogID), nullableProductionText(prepared.PredecessorReceiptSHA256),
			prepared.WithheldSelectionSHA256, prepared.PolicySHA256, players.SHA256,
			digestProductionBytes(producedJSON), producedJSON, rowsDigest); err != nil {
			return fmt.Errorf("creating privilege log draft: %w", err)
		}
		if err = insertPrivilegeRows(ctx, tx, prepared.LogID, prepared.Revision, rows); err != nil {
			return err
		}
		result.Generation = 1
		return recordProductionOperation(ctx, tx, prepared.OperationID, productionOperationDraft,
			prepared.RequestSHA256, result)
	})
	return result.Generation, err
}

func (s *Store) ReplacePrivilegeLogRows(ctx context.Context, update PrivilegeLogRowUpdate) (int64, error) {
	var result productionGenerationReceipt
	requestDigest, err := digestProductionValue(struct {
		LogID              string `json:"log_id"`
		Revision           int64  `json:"revision"`
		ExpectedGeneration int64  `json:"expected_generation"`
		RowsSHA256         string `json:"rows_sha256"`
	}{update.LogID, update.Revision, update.ExpectedGeneration, privilegeRowsDigest(update.Rows)})
	if err != nil || !validPreparedOperation(update.OperationID, requestDigest) || update.ExpectedGeneration < 1 {
		return 0, invalidProductionStorage("invalid privilege row update")
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if replay, ok, replayErr := productionOperationReplay[productionGenerationReceipt](ctx, tx,
			update.OperationID, productionOperationRows, requestDigest); replayErr != nil || ok {
			result = replay
			return replayErr
		}
		rows, rowsDigest, normalizeErr := normalizePrivilegeRows(update.Rows)
		if normalizeErr != nil {
			return normalizeErr
		}
		if err := requireMutablePrivilegeDraft(ctx, tx, update.LogID, update.Revision, update.ExpectedGeneration); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM production_privilege_log_validations WHERE log_id=? AND revision=?`, update.LogID, update.Revision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM production_privilege_log_approvals WHERE log_id=? AND revision=?`, update.LogID, update.Revision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM production_privilege_log_rows WHERE log_id=? AND revision=?`, update.LogID, update.Revision); err != nil {
			return err
		}
		if err := insertPrivilegeRows(ctx, tx, update.LogID, update.Revision, rows); err != nil {
			return err
		}
		result.Generation = update.ExpectedGeneration + 1
		changed, err := tx.ExecContext(ctx, `UPDATE production_privilege_log_drafts
			SET generation=?,rows_sha256=? WHERE log_id=? AND revision=? AND generation=?`,
			result.Generation, rowsDigest, update.LogID, update.Revision, update.ExpectedGeneration)
		if err != nil {
			return err
		}
		count, err := changed.RowsAffected()
		if err != nil || count != 1 {
			return staleProductionPrivilege(update.LogID)
		}
		return recordProductionOperation(ctx, tx, update.OperationID, productionOperationRows, requestDigest, result)
	})
	return result.Generation, err
}

func (s *Store) BindPrivilegeLogApproval(ctx context.Context, binding PrivilegeLogApprovalBinding) error {
	evaluationJSON, evaluationDigest, err := documentproduction.CanonicalApprovalEvaluation(binding.Evaluation)
	requestDigest, digestErr := digestProductionValue(struct {
		LogID              string `json:"log_id"`
		Revision           int64  `json:"revision"`
		ExpectedGeneration int64  `json:"expected_generation"`
		ApprovalID         string `json:"approval_id"`
		EvaluationSHA256   string `json:"evaluation_sha256"`
	}{binding.LogID, binding.Revision, binding.ExpectedGeneration, binding.ApprovalID, binding.Evaluation.SHA256})
	if err != nil || digestErr != nil || binding.Evaluation.SHA256 != evaluationDigest ||
		!validPreparedOperation(binding.OperationID, requestDigest) || binding.ExpectedGeneration < 1 {
		return invalidProductionStorage("invalid privilege approval binding")
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, ok, replayErr := productionOperationReplay[productionGenerationReceipt](ctx, tx,
			binding.OperationID, productionOperationApprovalBind, requestDigest); replayErr != nil || ok {
			return replayErr
		}
		if err := requireMutablePrivilegeDraft(ctx, tx, binding.LogID, binding.Revision, binding.ExpectedGeneration); err != nil {
			return err
		}
		stored, err := loadStoredPrivilegeLog(ctx, tx, binding.LogID, binding.Revision)
		if err != nil {
			return err
		}
		grant, subject, _, err := loadProductionApproval(ctx, tx, binding.ApprovalID)
		if err != nil {
			return err
		}
		events, err := loadProductionApprovalEvents(ctx, tx, binding.ApprovalID)
		if err != nil {
			return err
		}
		evaluatedAt, err := time.Parse(time.RFC3339Nano, binding.Evaluation.EvaluatedAt)
		if err != nil {
			return invalidProductionStorage("invalid privilege approval evaluation time")
		}
		currentEvaluation, err := productionservice.EvaluateRequiredApproval(
			stored.Policy, subject, &grant, events, evaluatedAt)
		if err != nil {
			return err
		}
		if stored.Validation == nil || !stored.Policy.Approval.Required ||
			grant.SHA256 != binding.Evaluation.ApprovalSHA256 ||
			subject.SetID != stored.Withheld.SetID || subject.Revision != stored.Withheld.Revision ||
			subject.WithheldSelectionSHA256 != stored.Withheld.SHA256 ||
			subject.PrivilegeLogInputsSHA256 != stored.Validation.Validation.InputsSHA256 ||
			binding.Evaluation.SubjectSHA256 != grant.SubjectSHA256 ||
			binding.Evaluation.SHA256 != currentEvaluation.SHA256 {
			return invalidProductionStorage("privilege approval does not bind validated authority")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO production_privilege_log_approvals
			(log_id,revision,approval_id,evaluation_sha256,canonical_json) VALUES(?,?,?,?,?)
			ON CONFLICT(log_id,revision) DO UPDATE SET approval_id=excluded.approval_id,
			 evaluation_sha256=excluded.evaluation_sha256,canonical_json=excluded.canonical_json`,
			binding.LogID, binding.Revision, binding.ApprovalID, evaluationDigest, evaluationJSON); err != nil {
			return err
		}
		return recordProductionOperation(ctx, tx, binding.OperationID, productionOperationApprovalBind,
			requestDigest, productionGenerationReceipt{Generation: binding.ExpectedGeneration})
	})
}

func (s *Store) ValidatePrivilegeLog(ctx context.Context, request productionservice.PrivilegeLogValidationRequest, build productionservice.PrivilegeLogValidationBuilder) (productionservice.PreparedPrivilegeLogValidation, error) {
	var result productionservice.PreparedPrivilegeLogValidation
	requestDigest, err := productionValidationRequestDigest(request)
	if err != nil {
		return result, err
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if replay, ok, replayErr := productionOperationReplay[productionservice.PreparedPrivilegeLogValidation](ctx, tx,
			request.OperationID, productionOperationValidation, requestDigest); replayErr != nil || ok {
			result = replay
			return replayErr
		}
		if build == nil {
			return invalidProductionStorage("nil privilege validation builder")
		}
		if err := requireMutablePrivilegeDraft(ctx, tx, request.LogID, request.Revision, request.ExpectedGeneration); err != nil {
			return err
		}
		stored, err := loadStoredPrivilegeLog(ctx, tx, request.LogID, request.Revision)
		if err != nil {
			return err
		}
		result, err = build(stored)
		if err != nil {
			return err
		}
		if result.OperationID != request.OperationID || result.RequestSHA256 != requestDigest ||
			result.DraftGeneration != stored.Generation ||
			result.Validation.Inputs.LogID != stored.LogID ||
			result.Validation.Inputs.Revision != stored.Revision ||
			result.Validation.RowsSHA256 != privilegeRowsDigest(stored.Rows) {
			return invalidProductionStorage("invalid privilege validation result")
		}
		validationJSON, err := canonical.Marshal(result)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM production_privilege_log_validations
			WHERE log_id=? AND revision=?`, request.LogID, request.Revision); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM production_privilege_log_approvals
			WHERE log_id=? AND revision=?`, request.LogID, request.Revision); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO production_privilege_log_validations
			(log_id,revision,operation_id,request_sha256,draft_generation,inputs_sha256,rows_sha256,canonical_json)
			VALUES(?,?,?,?,?,?,?,?)`, request.LogID, request.Revision, request.OperationID,
			requestDigest, result.DraftGeneration, result.Validation.InputsSHA256,
			result.Validation.RowsSHA256, validationJSON); err != nil {
			return err
		}
		return recordProductionOperation(ctx, tx, request.OperationID, productionOperationValidation,
			requestDigest, result)
	})
	return result, err
}

func (s *Store) FreezePrivilegeLog(ctx context.Context, request productionservice.PrivilegeLogFreezeRequest, build productionservice.PrivilegeLogFreezeBuilder) (documentproduction.PrivilegeLogReceipt, error) {
	var result documentproduction.PrivilegeLogReceipt
	requestDigest, err := productionFreezeRequestDigest(request)
	if err != nil {
		return result, err
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if replay, ok, replayErr := productionOperationReplay[documentproduction.PrivilegeLogReceipt](ctx, tx,
			request.OperationID, productionOperationFreeze, requestDigest); replayErr != nil || ok {
			result = replay
			return replayErr
		}
		if build == nil {
			return invalidProductionStorage("nil privilege freeze builder")
		}
		if err := requireMutablePrivilegeDraft(ctx, tx, request.LogID, request.Revision, request.ExpectedGeneration); err != nil {
			return err
		}
		stored, err := loadStoredPrivilegeLog(ctx, tx, request.LogID, request.Revision)
		if err != nil {
			return err
		}
		var builtRequest string
		result, builtRequest, err = build(stored)
		if err != nil {
			return err
		}
		if builtRequest != requestDigest || documentproduction.ValidatePrivilegeLogReceipt(result) != nil ||
			result.LogID != request.LogID || result.Revision != request.Revision {
			return invalidProductionStorage("invalid privilege freeze result")
		}
		receiptJSON, _, err := documentproduction.CanonicalPrivilegeLogReceipt(result)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO production_privilege_log_receipts
			(log_id,revision,sha256,canonical_json) VALUES(?,?,?,?)`,
			result.LogID, result.Revision, result.SHA256, receiptJSON); err != nil {
			return fmt.Errorf("freezing privilege log: %w", err)
		}
		return recordProductionOperation(ctx, tx, request.OperationID, productionOperationFreeze,
			requestDigest, result)
	})
	return result, err
}

func (s *Store) PutPrivilegeLogAttachment(ctx context.Context, prepared productionservice.PreparedPrivilegeLogAttachment) (documentproduction.PrivilegeLogAttachmentReceipt, error) {
	var result documentproduction.PrivilegeLogAttachmentReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if replay, ok, err := productionOperationReplay[documentproduction.PrivilegeLogAttachmentReceipt](ctx, tx,
			prepared.OperationID, productionOperationAttachment, prepared.RequestSHA256); err != nil || ok {
			result = replay
			return err
		}
		canonicalJSON, digest, err := documentproduction.CanonicalPrivilegeLogAttachment(prepared.Receipt)
		if err != nil || !validPreparedOperation(prepared.OperationID, prepared.RequestSHA256) ||
			digest != prepared.Receipt.SHA256 || digest != prepared.RequestSHA256 ||
			!bytes.Equal(canonicalJSON, prepared.Canonical) {
			return invalidProductionStorage("invalid privilege attachment")
		}
		if err = requireProductionDigest(ctx, tx, "production_privilege_log_receipts", "sha256",
			prepared.Receipt.PrivilegeLogReceiptSHA256); err != nil {
			return err
		}
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM production_privilege_log_attachments WHERE attachment_id=? OR sha256=?)`,
			prepared.Receipt.ID, digest).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return changedProductionPayload(prepared.Receipt.ID)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO production_privilege_log_attachments
			(attachment_id,privilege_log_receipt_sha256,sha256,canonical_json) VALUES(?,?,?,?)`,
			prepared.Receipt.ID, prepared.Receipt.PrivilegeLogReceiptSHA256, digest, canonicalJSON); err != nil {
			return fmt.Errorf("recording privilege log attachment: %w", err)
		}
		result = prepared.Receipt
		return recordProductionOperation(ctx, tx, prepared.OperationID, productionOperationAttachment,
			prepared.RequestSHA256, result)
	})
	return result, err
}

func (s *Store) PrivilegeLogReceipt(ctx context.Context, logID string, revision int64) (documentproduction.PrivilegeLogReceipt, error) {
	var raw []byte
	var digest string
	err := s.db.QueryRowContext(ctx, `SELECT canonical_json,sha256 FROM production_privilege_log_receipts
		WHERE log_id=? AND revision=?`, logID, revision).Scan(&raw, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return documentproduction.PrivilegeLogReceipt{}, ErrNotFound
	}
	if err != nil {
		return documentproduction.PrivilegeLogReceipt{}, err
	}
	return decodePrivilegeReceipt(raw, digest)
}

func (s *Store) PrivilegeLogPublicRows(ctx context.Context, logID string, revision int64) ([]documentproduction.PrivilegePublicRow, error) {
	rows, err := loadPrivilegeRows(ctx, s.db, logID, revision)
	if err != nil {
		return nil, err
	}
	return documentproduction.PublicPrivilegeRows(rows), nil
}

func (s *Store) ProductionApprovalPublic(ctx context.Context, approvalID string) (documentproduction.ApprovalPublicGrant, []documentproduction.ApprovalPublicEvent, error) {
	grant, _, _, err := loadProductionApproval(ctx, s.db, approvalID)
	if err != nil {
		return documentproduction.ApprovalPublicGrant{}, nil, err
	}
	events, err := loadProductionApprovalEvents(ctx, s.db, approvalID)
	if err != nil {
		return documentproduction.ApprovalPublicGrant{}, nil, err
	}
	return documentproduction.PublicApprovalGrant(grant), documentproduction.PublicApprovalEvents(events), nil
}

func loadStoredPrivilegeLog(ctx context.Context, q metadataQuerier, logID string, revision int64) (productionservice.StoredPrivilegeLog, error) {
	var stored productionservice.StoredPrivilegeLog
	var predecessor, withheldDigest, policyDigest, playersDigest string
	var producedJSON []byte
	err := q.QueryRowContext(ctx, `SELECT log_id,revision,generation,COALESCE(predecessor_log_id,''),
		withheld_selection_sha256,policy_sha256,players_sha256,produced_json
		FROM production_privilege_log_drafts WHERE log_id=? AND revision=?`, logID, revision).Scan(
		&stored.LogID, &stored.Revision, &stored.Generation, &predecessor,
		&withheldDigest, &policyDigest, &playersDigest, &producedJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return stored, ErrNotFound
	}
	if err != nil {
		return stored, err
	}
	stored.PredecessorID = predecessor
	stored.Policy, err = loadProductionPolicyByDigest(ctx, q, policyDigest)
	if err != nil {
		return stored, err
	}
	stored.Withheld, err = loadProductionWithheld(ctx, q, withheldDigest)
	if err != nil {
		return stored, err
	}
	stored.Players, err = loadProductionPlayers(ctx, q, playersDigest)
	if err != nil {
		return stored, err
	}
	stored.Produced, err = canonical.Decode[[]redaction.Member](producedJSON)
	if err != nil {
		return stored, invalidProductionStorage("stored produced selection is not canonical")
	}
	stored.Rows, err = loadPrivilegeRows(ctx, q, logID, revision)
	if err != nil {
		return stored, err
	}
	var validationJSON []byte
	err = q.QueryRowContext(ctx, `SELECT canonical_json FROM production_privilege_log_validations
		WHERE log_id=? AND revision=?`, logID, revision).Scan(&validationJSON)
	if err == nil {
		validation, decodeErr := canonical.Decode[productionservice.PreparedPrivilegeLogValidation](validationJSON)
		if decodeErr != nil {
			return stored, decodeErr
		}
		stored.Validation = &validation
	} else if !errors.Is(err, sql.ErrNoRows) {
		return stored, err
	}
	var approvalID string
	var evaluationDigest string
	var evaluationJSON []byte
	err = q.QueryRowContext(ctx, `SELECT approval_id,evaluation_sha256,canonical_json FROM production_privilege_log_approvals
		WHERE log_id=? AND revision=?`, logID, revision).Scan(&approvalID, &evaluationDigest, &evaluationJSON)
	if err == nil {
		grant, subject, _, loadErr := loadProductionApproval(ctx, q, approvalID)
		if loadErr != nil {
			return stored, loadErr
		}
		evaluation, loadErr := decodeApprovalEvaluation(evaluationJSON, evaluationDigest)
		if loadErr != nil {
			return stored, loadErr
		}
		events, loadErr := loadProductionApprovalEvents(ctx, q, approvalID)
		if loadErr != nil {
			return stored, loadErr
		}
		stored.ApprovalGrant = &grant
		stored.ApprovalSubject = &subject
		stored.ApprovalEvaluation = &evaluation
		stored.ApprovalEvents = events
	} else if !errors.Is(err, sql.ErrNoRows) {
		return stored, err
	}
	return stored, nil
}

func loadProductionPolicyByDigest(ctx context.Context, q metadataQuerier, digest string) (documentproduction.PolicyVersion, error) {
	var raw []byte
	var storedDigest string
	err := q.QueryRowContext(ctx, `SELECT canonical_json,sha256 FROM production_policy_versions WHERE sha256=?`, digest).Scan(&raw, &storedDigest)
	if err != nil {
		return documentproduction.PolicyVersion{}, productionRelationError("production policy", err)
	}
	return decodeProductionPolicy(raw, storedDigest)
}

func loadProductionPlayers(ctx context.Context, q metadataQuerier, digest string) (documentproduction.PlayersSnapshot, error) {
	var raw []byte
	var storedDigest string
	err := q.QueryRowContext(ctx, `SELECT canonical_json,sha256 FROM production_players_snapshots WHERE sha256=?`, digest).Scan(&raw, &storedDigest)
	if err != nil {
		return documentproduction.PlayersSnapshot{}, productionRelationError("players snapshot", err)
	}
	return decodePlayersSnapshot(raw, storedDigest)
}

func loadProductionWithheld(ctx context.Context, q metadataQuerier, digest string) (documentproduction.WithheldSelection, error) {
	var raw []byte
	var storedDigest string
	err := q.QueryRowContext(ctx, `SELECT canonical_json,sha256 FROM production_withheld_selections WHERE sha256=?`, digest).Scan(&raw, &storedDigest)
	if err != nil {
		return documentproduction.WithheldSelection{}, productionRelationError("withheld selection", err)
	}
	return decodeWithheldSelection(raw, storedDigest)
}

func loadProductionApproval(ctx context.Context, q metadataQuerier, approvalID string) (
	documentproduction.ApprovalGrant, documentproduction.ApprovalSubject,
	documentproduction.ApprovalAuthority, error,
) {
	var subjectJSON, authorityJSON, grantJSON []byte
	var subjectDigest, authorityDigest, grantDigest string
	err := q.QueryRowContext(ctx, `SELECT subject_json,subject_sha256,authority_json,authority_sha256,
		grant_json,grant_sha256 FROM production_approval_grants WHERE approval_id=?`, approvalID).Scan(
		&subjectJSON, &subjectDigest, &authorityJSON, &authorityDigest, &grantJSON, &grantDigest)
	if err != nil {
		return documentproduction.ApprovalGrant{}, documentproduction.ApprovalSubject{},
			documentproduction.ApprovalAuthority{}, productionRelationError("production approval", err)
	}
	subject, err := decodeApprovalSubject(subjectJSON, subjectDigest)
	if err != nil {
		return documentproduction.ApprovalGrant{}, subject, documentproduction.ApprovalAuthority{}, err
	}
	authority, err := decodeApprovalAuthority(authorityJSON, authorityDigest)
	if err != nil {
		return documentproduction.ApprovalGrant{}, subject, authority, err
	}
	grant, err := decodeApprovalGrant(grantJSON, grantDigest)
	return grant, subject, authority, err
}

func loadProductionApprovalEvents(ctx context.Context, q metadataQuerier, approvalID string) ([]documentproduction.ApprovalEvent, error) {
	rows, err := q.QueryContext(ctx, `SELECT canonical_json,event_sha256 FROM production_approval_events
		WHERE approval_id=? ORDER BY event_id`, approvalID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []documentproduction.ApprovalEvent{}
	for rows.Next() {
		var raw []byte
		var digest string
		if err := rows.Scan(&raw, &digest); err != nil {
			return nil, err
		}
		if digestProductionBytes(raw) != digest {
			return nil, changedProductionPayload(approvalID)
		}
		event, err := canonical.Decode[documentproduction.ApprovalEvent](raw)
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	encoded, _, err := documentproduction.CanonicalApprovalEvents(result)
	if err != nil {
		return nil, err
	}
	return canonical.Decode[[]documentproduction.ApprovalEvent](encoded)
}

func loadPrivilegeRows(ctx context.Context, q metadataQuerier, logID string, revision int64) ([]documentproduction.PrivilegeRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT canonical_json FROM production_privilege_log_rows
		WHERE log_id=? AND revision=? ORDER BY row_ordinal`, logID, revision)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []documentproduction.PrivilegeRow{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		row, err := canonical.Decode[documentproduction.PrivilegeRow](raw)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_, digest, err := normalizePrivilegeRows(result)
	if err != nil {
		return nil, err
	}
	var expected string
	if err := q.QueryRowContext(ctx, `SELECT rows_sha256 FROM production_privilege_log_drafts
		WHERE log_id=? AND revision=?`, logID, revision).Scan(&expected); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if digest != expected {
		return nil, changedProductionPayload(logID)
	}
	return result, nil
}

func insertPrivilegeRows(ctx context.Context, tx *sql.Tx, logID string, revision int64, rows []documentproduction.PrivilegeRow) error {
	for index, row := range rows {
		raw, err := canonical.Marshal(row)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO production_privilege_log_rows
			(log_id,revision,row_ordinal,row_id,canonical_json) VALUES(?,?,?,?,?)`,
			logID, revision, index+1, row.ID, raw); err != nil {
			return err
		}
	}
	return nil
}

func normalizePrivilegeRows(rows []documentproduction.PrivilegeRow) ([]documentproduction.PrivilegeRow, string, error) {
	raw, digest, err := documentproduction.CanonicalPrivilegeRows(rows)
	if err != nil {
		return nil, "", err
	}
	normalized, err := canonical.Decode[[]documentproduction.PrivilegeRow](raw)
	return normalized, digest, err
}

func privilegeRowsDigest(rows []documentproduction.PrivilegeRow) string {
	_, digest, err := documentproduction.CanonicalPrivilegeRows(rows)
	if err != nil {
		return ""
	}
	return digest
}

func requireMutablePrivilegeDraft(ctx context.Context, q metadataQuerier, logID string, revision, generation int64) error {
	var current int64
	var frozen int
	err := q.QueryRowContext(ctx, `SELECT d.generation,EXISTS(
		SELECT 1 FROM production_privilege_log_receipts r WHERE r.log_id=d.log_id AND r.revision=d.revision)
		FROM production_privilege_log_drafts d WHERE d.log_id=? AND d.revision=?`, logID, revision).Scan(&current, &frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if frozen != 0 || current != generation {
		return staleProductionPrivilege(logID)
	}
	return nil
}

func productionOperationReplay[T any](ctx context.Context, q metadataQuerier, operationID, kind, requestDigest string) (T, bool, error) {
	var zero T
	if err := validateUUIDv4(operationID); err != nil || !canonical.IsSHA256Hex(requestDigest) {
		return zero, false, invalidProductionStorage("invalid production operation")
	}
	var storedKind, storedRequest, responseDigest string
	var response []byte
	err := q.QueryRowContext(ctx, `SELECT kind,request_sha256,response_sha256,response_json
		FROM production_operation_receipts WHERE operation_id=?`, operationID).Scan(
		&storedKind, &storedRequest, &responseDigest, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, err
	}
	if storedKind != kind || storedRequest != requestDigest {
		return zero, false, changedProductionPayload(operationID)
	}
	if digestProductionBytes(response) != responseDigest {
		return zero, false, changedProductionPayload(operationID)
	}
	decoded, err := canonical.Decode[T](response)
	return decoded, err == nil, err
}

func recordProductionOperation(ctx context.Context, tx *sql.Tx, operationID, kind, requestDigest string, response any) error {
	raw, err := canonical.Marshal(response)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO production_operation_receipts
		(operation_id,kind,request_sha256,response_sha256,response_json) VALUES(?,?,?,?,?)`,
		operationID, kind, requestDigest, digestProductionBytes(raw), raw)
	return err
}

func decodeProductionPolicy(raw []byte, digest string) (documentproduction.PolicyVersion, error) {
	var value documentproduction.PolicyVersion
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return value, err
	}
	canonicalJSON, actual, err := documentproduction.CanonicalPolicyVersion(value)
	if err != nil || actual != digest || !bytes.Equal(raw, canonicalJSON) {
		return value, changedProductionPayload(value.ID)
	}
	value.SHA256 = digest
	return value, documentproduction.ValidatePolicyVersion(value)
}

func decodePlayersSnapshot(raw []byte, digest string) (documentproduction.PlayersSnapshot, error) {
	var value documentproduction.PlayersSnapshot
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return value, err
	}
	canonicalJSON, actual, err := documentproduction.CanonicalPlayersSnapshot(value)
	if err != nil || actual != digest || !bytes.Equal(raw, canonicalJSON) {
		return value, changedProductionPayload(value.ID)
	}
	value.SHA256 = digest
	return value, documentproduction.ValidatePlayersSnapshot(value)
}

func decodeWithheldSelection(raw []byte, digest string) (documentproduction.WithheldSelection, error) {
	var value documentproduction.WithheldSelection
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return value, err
	}
	canonicalJSON, actual, err := documentproduction.CanonicalWithheldSelection(value)
	if err != nil || actual != digest || !bytes.Equal(raw, canonicalJSON) {
		return value, changedProductionPayload(value.ID)
	}
	value.SHA256 = digest
	return value, documentproduction.ValidateWithheldSelection(value)
}

func decodeApprovalSubject(raw []byte, digest string) (documentproduction.ApprovalSubject, error) {
	var value documentproduction.ApprovalSubject
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return value, err
	}
	canonicalJSON, actual, err := documentproduction.CanonicalApprovalSubject(value)
	if err != nil || actual != digest || !bytes.Equal(raw, canonicalJSON) {
		return value, changedProductionPayload(value.SetID)
	}
	return value, nil
}

func decodeApprovalAuthority(raw []byte, digest string) (documentproduction.ApprovalAuthority, error) {
	var value documentproduction.ApprovalAuthority
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return value, err
	}
	canonicalJSON, actual, err := documentproduction.CanonicalApprovalAuthority(value)
	if err != nil || actual != digest || !bytes.Equal(raw, canonicalJSON) {
		return value, changedProductionPayload(value.PrincipalID)
	}
	return value, nil
}

func decodeApprovalGrant(raw []byte, digest string) (documentproduction.ApprovalGrant, error) {
	var value documentproduction.ApprovalGrant
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return value, err
	}
	canonicalJSON, actual, err := documentproduction.CanonicalApprovalGrant(value)
	if err != nil || actual != digest || !bytes.Equal(raw, canonicalJSON) {
		return value, changedProductionPayload(value.ID)
	}
	value.SHA256 = digest
	return value, documentproduction.ValidateApprovalGrant(value)
}

func decodeApprovalEvaluation(raw []byte, storedDigest string) (documentproduction.ApprovalEvaluation, error) {
	var value documentproduction.ApprovalEvaluation
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return value, err
	}
	canonicalJSON, digest, err := documentproduction.CanonicalApprovalEvaluation(value)
	if err != nil || digest != storedDigest || !bytes.Equal(raw, canonicalJSON) {
		return value, changedProductionPayload(value.ApprovalSHA256)
	}
	value.SHA256 = storedDigest
	return value, nil
}

func decodePrivilegeReceipt(raw []byte, digest string) (documentproduction.PrivilegeLogReceipt, error) {
	var value documentproduction.PrivilegeLogReceipt
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return value, err
	}
	canonicalJSON, actual, err := documentproduction.CanonicalPrivilegeLogReceipt(value)
	if err != nil || actual != digest || !bytes.Equal(raw, canonicalJSON) {
		return value, changedProductionPayload(value.LogID)
	}
	value.SHA256 = digest
	return value, documentproduction.ValidatePrivilegeLogReceipt(value)
}

func validatePreparedPrivilegeDraft(value productionservice.PreparedPrivilegeLogDraft) error {
	if !validPreparedOperation(value.OperationID, value.RequestSHA256) || validateUUIDv4(value.LogID) != nil ||
		value.Revision < 1 || !canonical.IsSHA256Hex(value.WithheldSelectionSHA256) ||
		!canonical.IsSHA256Hex(value.PolicySHA256) {
		return invalidProductionStorage("invalid prepared privilege draft")
	}
	digest, err := digestProductionValue(struct {
		LogID                    string `json:"log_id"`
		Revision                 int64  `json:"revision"`
		PredecessorLogID         string `json:"predecessor_log_id,omitzero"`
		PredecessorReceiptSHA256 string `json:"predecessor_receipt_sha256,omitzero"`
		WithheldSelectionSHA256  string `json:"withheld_selection_sha256"`
		PolicySHA256             string `json:"policy_sha256"`
	}{value.LogID, value.Revision, value.PredecessorLogID, value.PredecessorReceiptSHA256,
		value.WithheldSelectionSHA256, value.PolicySHA256})
	if err != nil || digest != value.RequestSHA256 {
		return invalidProductionStorage("invalid privilege draft request digest")
	}
	if (value.PredecessorLogID == "") != (value.PredecessorReceiptSHA256 == "") {
		return invalidProductionStorage("incomplete privilege predecessor")
	}
	if value.PredecessorLogID != "" && (validateUUIDv4(value.PredecessorLogID) != nil ||
		!canonical.IsSHA256Hex(value.PredecessorReceiptSHA256) || value.PredecessorLogID == value.LogID) {
		return invalidProductionStorage("invalid privilege predecessor")
	}
	return nil
}

func productionValidationRequestDigest(request productionservice.PrivilegeLogValidationRequest) (string, error) {
	return digestProductionValue(struct {
		LogID              string `json:"log_id"`
		Revision           int64  `json:"revision"`
		ExpectedGeneration int64  `json:"expected_generation"`
		ValidatedAt        string `json:"validated_at"`
	}{request.LogID, request.Revision, request.ExpectedGeneration, request.ValidatedAt.Format(timeRFC3339Nano)})
}

func productionFreezeRequestDigest(request productionservice.PrivilegeLogFreezeRequest) (string, error) {
	return digestProductionValue(struct {
		LogID                            string `json:"log_id"`
		Revision                         int64  `json:"revision"`
		ExpectedGeneration               int64  `json:"expected_generation"`
		ExpectedInputsSHA256             string `json:"expected_inputs_sha256"`
		ExpectedApprovalEvaluationSHA256 string `json:"expected_approval_evaluation_sha256,omitzero"`
		FrozenAt                         string `json:"frozen_at"`
	}{request.LogID, request.Revision, request.ExpectedGeneration, request.ExpectedInputsSHA256,
		request.ExpectedApprovalEvaluationSHA256, request.FrozenAt.Format(timeRFC3339Nano)})
}

const timeRFC3339Nano = "2006-01-02T15:04:05.999999999Z07:00"

func digestProductionValue(value any) (string, error) {
	raw, err := canonical.Marshal(value)
	if err != nil {
		return "", err
	}
	return digestProductionBytes(raw), nil
}

func digestProductionBytes(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func validPreparedOperation(operationID, requestDigest string) bool {
	return validateUUIDv4(operationID) == nil && canonical.IsSHA256Hex(requestDigest)
}

func requireProductionDigest(ctx context.Context, q metadataQuerier, table, column, digest string) error {
	if !canonical.IsSHA256Hex(digest) {
		return invalidProductionStorage("invalid production digest")
	}
	var exists bool
	query := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE %s=?)`, table, column)
	if err := q.QueryRowContext(ctx, query, digest).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func nonNilProduced(values []redaction.Member) []redaction.Member {
	if values == nil {
		return []redaction.Member{}
	}
	return slices.Clone(values)
}

func nullableProductionText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func productionRelationError(subject string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", subject, ErrNotFound)
	}
	return err
}

func invalidProductionStorage(detail string) *documentproduction.Problem {
	return &documentproduction.Problem{Code: documentproduction.ProblemInvalidContract, Detail: detail}
}

func changedProductionPayload(subject string) *documentproduction.Problem {
	return &documentproduction.Problem{Code: documentproduction.ProblemChangedPayload,
		Detail: "production operation payload changed", SubjectID: subject}
}

func staleProductionPrivilege(logID string) *documentproduction.Problem {
	return &documentproduction.Problem{Code: documentproduction.ProblemPrivilegeLogStale,
		Detail: "privilege log authority changed", SubjectID: logID}
}

var _ productionservice.PrivilegeLogStore = (*Store)(nil)
