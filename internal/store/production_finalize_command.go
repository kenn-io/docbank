package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

const productionOperationFinalizeCommand = "finalize_command"

// ProductionFinalizeCommand selects an existing numbering namespace and a
// stable snapshot identity. Its operation ID binds the entire request.
type ProductionFinalizeCommand struct {
	SetID       string `json:"set_id"`
	Revision    int64  `json:"revision"`
	ETag        int64  `json:"etag"`
	OperationID string `json:"operation_id"`
	NamespaceID string `json:"namespace_id"`
	SnapshotID  string `json:"snapshot_id"`
	StartAt     int64  `json:"start_at"`
}

type ProductionFinalizationResult struct {
	Draft          redaction.Draft `json:"draft"`
	OperationID    string          `json:"operation_id"`
	NamespaceID    string          `json:"namespace_id"`
	SnapshotID     string          `json:"snapshot_id"`
	PreparedSHA256 string          `json:"prepared_sha256"`
	ReceiptSHA256  string          `json:"receipt_sha256"`
}

type productionFinalizeAdmission struct {
	Command                ProductionFinalizeCommand `json:"command"`
	PreparedAt             string                    `json:"prepared_at"`
	ExpectedRevisionSHA256 string                    `json:"expected_revision_sha256"`
}

func validProductionFinalizeCommand(command ProductionFinalizeCommand) bool {
	return validateUUIDv4(command.SetID) == nil && validateUUIDv4(command.OperationID) == nil &&
		validateUUIDv4(command.NamespaceID) == nil && validateUUIDv4(command.SnapshotID) == nil &&
		command.Revision > 0 && command.ETag > 0 && command.StartAt >= 0
}

func productionFinalizeDerivedID(kind, operationID string) string {
	sum := sha256.Sum256([]byte("production-finalize/v1\x00" + kind + "\x00" + operationID))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:16])
}

// FinalizeProductionDraft runs the stored input gates and locks one exact
// revision. The command admission commits before the gate so a retry uses the
// same observation time and digest after any later source or policy drift.
func (s *Store) FinalizeProductionDraft(ctx context.Context, actor string,
	command ProductionFinalizeCommand) (ProductionFinalizationResult, error) {
	if ctx == nil || !validProductionActor(actor) || !validProductionFinalizeCommand(command) {
		return ProductionFinalizationResult{}, ErrInvalidProduction
	}
	raw, err := canonical.Marshal(command)
	if err != nil {
		return ProductionFinalizationResult{}, err
	}
	requestSHA256 := digestProductionBytes(raw)
	var admitted productionFinalizeAdmission
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		replay, found, err := productionOperationReplay[productionFinalizeAdmission](ctx, tx,
			command.OperationID, productionOperationFinalizeCommand, requestSHA256)
		if err != nil {
			return err
		}
		if found {
			admitted = replay
			if err := validateProductionFinalizeAdmission(admitted, command); err != nil {
				return err
			}
			return validateProductionFinalizeAuditTx(ctx, tx, command.OperationID,
				command.SetID, command.Revision, requestSHA256)
		}
		draft, err := scanProductionDraft(tx.QueryRowContext(ctx,
			productionDraftSelect+` WHERE set_id=? AND revision=?`, command.SetID, command.Revision))
		if err != nil {
			return err
		}
		if draft.ETag != command.ETag || draft.State != "draft" {
			return ErrProductionRevisionConflict
		}
		if draft.NumberingRecipeID != redaction.BatesNumberingRecipeID {
			return ErrInvalidProduction
		}
		var namespaceCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM bates_namespaces WHERE namespace_id=?`,
			command.NamespaceID).Scan(&namespaceCount); err != nil {
			return err
		}
		if namespaceCount != 1 {
			return ErrBatesReservationConflict
		}
		preparedAt := time.Now().UTC()
		stored, err := s.LoadProductionGateSnapshot(ctx, tx, productionservice.PreparedInputRequest{
			SetID: command.SetID, Revision: command.Revision, PreparedAt: preparedAt})
		if err != nil {
			return err
		}
		if stored.Draft != draft {
			return ErrProductionRevisionConflict
		}
		revisionSHA, err := productionservice.ProductionRevisionSHA256(stored)
		if err != nil {
			return err
		}
		admitted = productionFinalizeAdmission{Command: command,
			PreparedAt: preparedAt.Format(time.RFC3339Nano), ExpectedRevisionSHA256: revisionSHA}
		if err := recordProductionOperation(ctx, tx, command.OperationID,
			productionOperationFinalizeCommand, requestSHA256, admitted); err != nil {
			return err
		}
		return recordProductionFinalizeAuditTx(ctx, tx, actor, admitted, requestSHA256)
	})
	if err != nil {
		return ProductionFinalizationResult{}, err
	}
	preparedAt, err := time.Parse(time.RFC3339Nano, admitted.PreparedAt)
	if err != nil {
		return ProductionFinalizationResult{}, ErrInvalidProduction
	}
	gateID := productionFinalizeDerivedID("gate", command.OperationID)
	gateStore, err := NewProductionGateStore(s, s.LoadProductionGateSnapshot)
	if err != nil {
		return ProductionFinalizationResult{}, err
	}
	authority, err := productionservice.RunPreparedInputGates(ctx, gateStore,
		productionservice.PreparedInputRequest{OperationID: gateID,
			ReceiptID: productionFinalizeDerivedID("receipt", command.OperationID),
			SetID:     command.SetID, Revision: command.Revision,
			ExpectedETag: command.ETag, ExpectedRevisionSHA256: admitted.ExpectedRevisionSHA256,
			PreparedAt: preparedAt})
	if err != nil {
		return ProductionFinalizationResult{}, err
	}
	if authority.Receipt == nil || !documentproduction.GateResultsPassed(authority.GateResults) {
		return ProductionFinalizationResult{}, ErrInvalidProduction
	}
	existing, err := s.LoadFinalizedProduction(ctx, command.SetID, command.Revision)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ProductionFinalizationResult{}, err
	}
	if errors.Is(err, ErrNotFound) {
		if _, err := s.SealProductionNumberingSnapshot(ctx, command.SnapshotID, gateID); err != nil {
			return ProductionFinalizationResult{}, err
		}
	}
	var draft redaction.Draft
	if err == nil {
		draft = existing.Draft
	} else {
		draft, err = s.ProductionDraft(ctx, command.SetID, command.Revision)
		if err != nil {
			return ProductionFinalizationResult{}, err
		}
		draft.State = "finalized"
	}
	if draft.ETag != command.ETag {
		return ProductionFinalizationResult{}, ErrProductionRevisionConflict
	}
	if err := s.FinalizeProductionRevision(ctx, productionservice.FinalizationRequest{
		Finalized:   productionservice.FinalizedProduction{Draft: draft, Authority: authority},
		OperationID: gateID, NamespaceID: command.NamespaceID,
		SnapshotID: command.SnapshotID, RecipeSHA256: draft.NumberingRecipeSHA256,
		StartAt: command.StartAt,
	}); err != nil {
		return ProductionFinalizationResult{}, err
	}
	return ProductionFinalizationResult{Draft: draft, OperationID: command.OperationID,
		NamespaceID: command.NamespaceID, SnapshotID: command.SnapshotID,
		PreparedSHA256: authority.Prepared.SHA256, ReceiptSHA256: authority.Receipt.SHA256}, nil
}

func recordProductionFinalizeAuditTx(ctx context.Context, tx *sql.Tx, actor string,
	admitted productionFinalizeAdmission, requestSHA256 string) error {
	raw, err := canonical.Marshal(admitted)
	if err != nil {
		return err
	}
	command := admitted.Command
	createdAt := nowRFC3339()
	if _, err := tx.ExecContext(ctx, `INSERT INTO production_operations(operation_id,set_id,actor,kind,request_sha256,receipt_json,created_at) VALUES(?,?,?,?,?,?,?)`,
		command.OperationID, command.SetID, actor, productionOperationFinalizeCommand, requestSHA256, raw, createdAt); err != nil {
		return fmt.Errorf("recording production finalize operation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO production_audit_evidence(operation_id,set_id,revision,actor,kind,request_sha256,receipt_sha256,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		command.OperationID, command.SetID, command.Revision, actor, productionOperationFinalizeCommand,
		requestSHA256, digestProductionBytes(raw), createdAt); err != nil {
		return fmt.Errorf("recording production finalize audit: %w", err)
	}
	return nil
}

func validateProductionFinalizeAuditTx(ctx context.Context, tx *sql.Tx, operationID, setID string,
	revision int64, requestSHA256 string) error {
	var actor, kind, storedSetID, storedSHA string
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT set_id,actor,kind,request_sha256,receipt_json FROM production_operations WHERE operation_id=?`,
		operationID).Scan(&storedSetID, &actor, &kind, &storedSHA, &raw); err != nil ||
		storedSetID != setID || kind != productionOperationFinalizeCommand || storedSHA != requestSHA256 ||
		!validProductionActor(actor) {
		return ErrInvalidProduction
	}
	return validateProductionOperationAuditTx(ctx, tx, operationID, setID, revision, actor, kind, requestSHA256, raw)
}

func validateProductionFinalizeAdmission(admitted productionFinalizeAdmission, command ProductionFinalizeCommand) error {
	if admitted.Command != command || !canonical.IsSHA256Hex(admitted.ExpectedRevisionSHA256) {
		return ErrInvalidProduction
	}
	preparedAt, err := time.Parse(time.RFC3339Nano, admitted.PreparedAt)
	if err != nil || preparedAt.Location() != time.UTC {
		return ErrInvalidProduction
	}
	return nil
}
