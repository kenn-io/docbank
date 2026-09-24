package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/canonical"
)

const productionOperationPreviewAdmission = "preview_admission"

// ProductionPreviewCommand binds one preview operation to the exact draft,
// member and page selected by its caller.
type ProductionPreviewCommand struct {
	SetID       string `json:"set_id"`
	Revision    int64  `json:"revision"`
	ETag        int64  `json:"etag"`
	OperationID string `json:"operation_id"`
	MemberID    string `json:"member_id"`
	Page        int    `json:"page"`
}

// ProductionPreviewAdmission is durable input authority, not a download
// ticket. Exact retries can issue fresh one-use tickets only after rendering
// again and comparing the staged input digest with this receipt.
type ProductionPreviewAdmission struct {
	Command            ProductionPreviewCommand `json:"command"`
	Actor              string                   `json:"actor"`
	PreviewInputSHA256 string                   `json:"preview_input_sha256"`
}

func validProductionPreviewCommand(command ProductionPreviewCommand) bool {
	return validateUUIDv4(command.SetID) == nil && validateUUIDv4(command.OperationID) == nil &&
		validateUUIDv4(command.MemberID) == nil && command.Revision > 0 && command.ETag > 0 && command.Page > 0
}

func validProductionPreviewAdmission(value ProductionPreviewAdmission) bool {
	return validProductionPreviewCommand(value.Command) && validProductionActor(value.Actor) &&
		canonical.IsSHA256Hex(value.PreviewInputSHA256)
}

// AdmitProductionDraftPreview retains a replay identity before a route issues
// ephemeral download tickets. The current draft and evidence must agree on
// first admission; a replay returns the original binding even after drift.
func (s *Store) AdmitProductionDraftPreview(ctx context.Context, actor string,
	command ProductionPreviewCommand) (ProductionPreviewAdmission, error) {
	if ctx == nil || !validProductionActor(actor) || !validProductionPreviewCommand(command) {
		return ProductionPreviewAdmission{}, ErrInvalidProduction
	}
	raw, err := canonical.Marshal(command)
	if err != nil {
		return ProductionPreviewAdmission{}, err
	}
	requestSHA := digestProductionBytes(raw)
	var admitted ProductionPreviewAdmission
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		replay, found, replayErr := productionOperationReplay[ProductionPreviewAdmission](ctx, tx,
			command.OperationID, productionOperationPreviewAdmission, requestSHA)
		if replayErr != nil {
			return replayErr
		}
		if found {
			if !validProductionPreviewAdmission(replay) || replay.Command != command || replay.Actor != actor {
				return ErrProductionOperationConflict
			}
			if err := validateProductionPreviewAuditTx(ctx, tx, replay, requestSHA); err != nil {
				return err
			}
			admitted = replay
			return nil
		}
		var used bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM production_operations WHERE operation_id=?)`,
			command.OperationID).Scan(&used); err != nil {
			return err
		}
		if used {
			return ErrProductionOperationConflict
		}
		input, err := s.selectProductionDraftPreviewInputTx(ctx, tx, command.SetID,
			command.Revision, command.ETag, command.MemberID)
		if err != nil {
			return err
		}
		if command.Page > len(input.Member.Resolved.Pages) {
			return ErrInvalidProduction
		}
		admitted = ProductionPreviewAdmission{Command: command, Actor: actor,
			PreviewInputSHA256: input.PreviewInputSHA256}
		if err := recordProductionOperation(ctx, tx, command.OperationID,
			productionOperationPreviewAdmission, requestSHA, admitted); err != nil {
			return fmt.Errorf("recording preview admission: %w", err)
		}
		return recordProductionPreviewAuditTx(ctx, tx, admitted, requestSHA)
	})
	if err != nil {
		return ProductionPreviewAdmission{}, err
	}
	return admitted, nil
}

func recordProductionPreviewAuditTx(ctx context.Context, tx *sql.Tx,
	value ProductionPreviewAdmission, requestSHA string) error {
	raw, err := canonical.Marshal(value)
	if err != nil {
		return err
	}
	command := value.Command
	createdAt := nowRFC3339()
	if _, err := tx.ExecContext(ctx, `INSERT INTO production_operations
		(operation_id,set_id,actor,kind,request_sha256,receipt_json,created_at) VALUES(?,?,?,?,?,?,?)`,
		command.OperationID, command.SetID, value.Actor, productionOperationPreviewAdmission,
		requestSHA, raw, createdAt); err != nil {
		return fmt.Errorf("recording preview operation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO production_audit_evidence
		(operation_id,set_id,revision,actor,kind,request_sha256,receipt_sha256,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		command.OperationID, command.SetID, command.Revision, value.Actor,
		productionOperationPreviewAdmission, requestSHA, digestProductionBytes(raw), createdAt); err != nil {
		return fmt.Errorf("recording preview audit: %w", err)
	}
	return nil
}

func validateProductionPreviewAuditTx(ctx context.Context, tx *sql.Tx,
	value ProductionPreviewAdmission, requestSHA string) error {
	command := value.Command
	var setID, actor, kind, storedSHA string
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT set_id,actor,kind,request_sha256,receipt_json
		FROM production_operations WHERE operation_id=?`, command.OperationID).
		Scan(&setID, &actor, &kind, &storedSHA, &raw)
	if err != nil || setID != command.SetID || actor != value.Actor ||
		kind != productionOperationPreviewAdmission || storedSHA != requestSHA {
		return errors.Join(ErrInvalidProduction, err)
	}
	encoded, err := canonical.Marshal(value)
	if err != nil || !bytes.Equal(raw, encoded) {
		return errors.Join(ErrInvalidProduction, err)
	}
	return validateProductionOperationAuditTx(ctx, tx, command.OperationID, command.SetID,
		command.Revision, actor, kind, requestSHA, raw)
}
