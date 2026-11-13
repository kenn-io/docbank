package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/docbank/internal/canonical"
)

// ErrMediaOperationConflict reports operation-ID reuse for another request.
var ErrMediaOperationConflict = errors.New("media operation conflict")

const (
	mediaOperationQueued     = "queued"
	mediaOperationSucceeded  = "succeeded"
	MediaOperationFailed     = "failed"
	mediaCoverageUnavailable = "unavailable"
)

// MediaOperation binds a caller mutation to one canonical request digest and
// its durable replay receipt.
type MediaOperation struct {
	ID, Principal, Verb, RequestSHA256, SourceID, ReceiptJSON string
}

// MediaOperationReceipt resolves an exact successful/queued replay before
// mutable admission checks. Wrong principals are indistinguishable from an
// unknown operation; changed request identity conflicts.
func (s *Store) MediaOperationReceipt(
	ctx context.Context, op MediaOperation,
) (string, error) {
	if err := validateMediaOperation(MediaOperation{ID: op.ID, Principal: op.Principal,
		Verb: op.Verb, RequestSHA256: op.RequestSHA256, SourceID: op.SourceID}); err != nil {
		return "", err
	}
	var principal, verb, digest, receipt string
	err := s.db.QueryRowContext(ctx, `SELECT principal,verb,request_sha256,receipt_json
		FROM media_operations WHERE operation_id=$1`, op.ID).Scan(&principal, &verb, &digest, &receipt)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && principal != op.Principal) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if verb != op.Verb || digest != op.RequestSHA256 {
		return "", ErrMediaOperationConflict
	}
	return receipt, nil
}

// RemoteRecordingReceipt returns the stored receipt of one remote-recording
// submission owned by principal. Unknown IDs, other principals, and other
// verbs are indistinguishable. A miss reports only this read.
func (s *Store) RemoteRecordingReceipt(ctx context.Context, principal, operationID string) (string, error) {
	if err := validateUUIDv4(operationID); err != nil {
		return "", fmt.Errorf("invalid media operation ID: %w", err)
	}
	var receipt string
	err := s.db.QueryRowContext(ctx, `SELECT receipt_json FROM media_operations
		WHERE operation_id=$1 AND principal=$2 AND verb='submit_remote_recording'`,
		operationID, storedString(s.postgresSchema != "", principal)).Scan(&receipt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return receipt, err
}

// withMediaOperation runs a local media mutation and stores its successful
// canonical receipt in the same logical transaction. Identical retries read
// that receipt without invoking mutate again.
func (s *Store) withMediaOperation(
	ctx context.Context, op MediaOperation, mutate func(*sql.Tx) (string, error),
) (string, error) {
	if err := validateMediaOperation(op); err != nil {
		return "", err
	}
	if mutate == nil {
		return "", errors.New("media operation mutation is required")
	}
	var receipt string
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var principal, verb, digest, storedReceipt string
		err := tx.QueryRowContext(ctx, `SELECT principal,verb,request_sha256,receipt_json
			FROM media_operations WHERE operation_id=$1`, op.ID).Scan(
			&principal, &verb, &digest, &storedReceipt)
		same := principal == op.Principal && verb == op.Verb && digest == op.RequestSHA256
		stored, found, err := replayReceipt(storedReceipt, err, same, ErrMediaOperationConflict)
		if err != nil || found {
			receipt = stored
			return err
		}
		receipt, err = mutate(tx)
		if err != nil {
			return err
		}
		if err := validateMediaReceipt(receipt); err != nil {
			return err
		}
		stamp := nowRFC3339()
		// Preserve admission order even when successive calls share a clock tick.
		// The timestamp survives metadata export and restore; row IDs do not.
		// Keep both forms indexable by principal and source on either backend.
		sourcePredicate := "source_id IS NULL"
		args := []any{storedString(s.postgresSchema != "", op.Principal)}
		if op.SourceID != "" {
			sourcePredicate = "source_id=$2"
			args = append(args, storedString(s.postgresSchema != "", op.SourceID))
		}
		var previous string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(created_at),'') FROM media_operations
			WHERE principal=$1 AND `+sourcePredicate, args...).Scan(&previous); err != nil {
			return err
		}
		if stamp <= previous {
			last, err := time.Parse(time.RFC3339Nano, previous)
			if err != nil {
				return fmt.Errorf("parsing previous media admission time: %w", err)
			}
			stamp = last.Add(time.Nanosecond).UTC().Format(timestampLayout)
		}
		var sourceID *string
		if op.SourceID != "" {
			sourceID = &op.SourceID
		}
		if err := insertMediaOperationTx(ctx, tx, metadataMediaOperation{OperationID: op.ID, Principal: op.Principal, Verb: op.Verb, RequestSHA256: op.RequestSHA256, SourceID: sourceID, ReceiptJSON: receipt, CreatedAt: stamp, UpdatedAt: stamp}, s.postgresSchema != ""); err != nil {
			if s.isUniqueViolation(err) {
				return ErrMediaOperationConflict
			}
			return fmt.Errorf("recording media operation: %w", err)
		}

		return nil
	})
	return receipt, err
}

func validateMediaOperation(op MediaOperation) error {
	if err := validateUUIDv4(op.ID); err != nil {
		return fmt.Errorf("invalid media operation ID: %w", err)
	}
	if err := validateBoundedMediaText("media operation principal", op.Principal, 256, false); err != nil {
		return err
	}
	if !validMediaOperationVerb(op.Verb) || !canonical.IsSHA256Hex(op.RequestSHA256) {
		return ErrMediaOperationConflict
	}
	if op.SourceID != "" {
		if err := validateBoundedMediaText("media operation source ID", op.SourceID, 256, false); err != nil {
			return err
		}
	}
	if op.ReceiptJSON != "" {
		return ErrMediaOperationConflict
	}
	return nil
}

func validMediaOperationVerb(verb string) bool {
	switch verb {
	case "submit_supplied_media", "submit_remote_recording", "declare_occurrence",
		"revoke_occurrence", "retry_media", "import_recording_artifact",
		"grant_media_acquisition", "revoke_media_acquisition":
		return true
	default:
		return false
	}
}

func validateMediaReceipt(receipt string) error {
	if len(receipt) == 0 || len(receipt) > 64<<10 {
		return errors.New("invalid media receipt")
	}
	canonicalReceipt, err := canonicalCatalogJSON(jsontext.Value(receipt), "media receipt")
	if err != nil || !bytes.Equal(canonicalReceipt, []byte(receipt)) {
		return errors.New("invalid media receipt")
	}
	return nil
}

// PostgreSQL JSON extraction rejects NUL in any receipt string. Keep query
// keys as derived byte columns while preserving the receipt's original bytes.
func insertMediaOperationTx(ctx context.Context, tx *sql.Tx, v metadataMediaOperation, postgres bool) error {
	columns := "operation_id,principal,verb,request_sha256,source_id,receipt_json,created_at,updated_at"
	args := []any{v.OperationID, storedString(postgres, v.Principal), v.Verb, v.RequestSHA256, storedOptionalString(postgres, v.SourceID), v.ReceiptJSON, v.CreatedAt, v.UpdatedAt}
	if postgres {
		version, state, err := mediaReceiptQueryKeys(v.ReceiptJSON)
		if err != nil {
			return err
		}

		columns += ",receipt_source_version_id,receipt_operation_state"
		args = append(args, []byte(version), []byte(state))
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO media_operations("+columns+") VALUES("+numberedPlaceholders(len(args), 0)+")", args...)
	return err
}

func mediaReceiptQueryKeys(raw string) (string, string, error) {
	var keys struct {
		SourceVersionID string `json:"source_version_id"`
		OperationState  string `json:"operation_state"`
	}
	err := json.Unmarshal([]byte(raw), &keys)
	return keys.SourceVersionID, keys.OperationState, err
}
