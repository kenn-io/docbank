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

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

const metadataProductionAuthorityType = "production_authority"

const (
	productionMetadataPolicy     = "policy"
	productionMetadataApproval   = "approval"
	productionMetadataEvent      = "approval_event"
	productionMetadataPlayers    = "players"
	productionMetadataWithheld   = "withheld"
	productionMetadataDraft      = "privilege_draft"
	productionMetadataRow        = "privilege_row"
	productionMetadataValidation = "privilege_validation"
	productionMetadataBinding    = "privilege_approval"
	productionMetadataReceipt    = "privilege_receipt"
	productionMetadataAttachment = "privilege_attachment"
	productionMetadataOperation  = "operation_receipt"
)

type metadataProductionAuthority struct {
	Type          string `json:"type"`
	Kind          string `json:"kind"`
	Key           string `json:"key"`
	CanonicalJSON []byte `json:"canonical_json" format:"byte"`
	Checksum      string `json:"checksum"`
}

type productionMetadataPolicyRow struct {
	PolicyID      string `json:"policy_id"`
	Version       int64  `json:"version"`
	SHA256        string `json:"sha256"`
	CanonicalJSON []byte `json:"canonical_json" format:"byte"`
}

type productionMetadataApprovalRow struct {
	ApprovalID      string `json:"approval_id"`
	SubjectSHA256   string `json:"subject_sha256"`
	SubjectJSON     []byte `json:"subject_json" format:"byte"`
	AuthoritySHA256 string `json:"authority_sha256"`
	AuthorityJSON   []byte `json:"authority_json" format:"byte"`
	GrantSHA256     string `json:"grant_sha256"`
	GrantJSON       []byte `json:"grant_json" format:"byte"`
}

type productionMetadataEventRow struct {
	EventID       string `json:"event_id"`
	ApprovalID    string `json:"approval_id"`
	EventSHA256   string `json:"event_sha256"`
	CanonicalJSON []byte `json:"canonical_json" format:"byte"`
}

type productionMetadataPlayersRow struct {
	SnapshotID    string `json:"snapshot_id"`
	Revision      int64  `json:"revision"`
	SHA256        string `json:"sha256"`
	CanonicalJSON []byte `json:"canonical_json" format:"byte"`
}

type productionMetadataWithheldRow struct {
	SelectionID   string `json:"selection_id"`
	SetID         string `json:"set_id"`
	Revision      int64  `json:"revision"`
	PolicySHA256  string `json:"policy_sha256"`
	SHA256        string `json:"sha256"`
	CanonicalJSON []byte `json:"canonical_json" format:"byte"`
}

type productionMetadataDraftRow struct {
	LogID                    string  `json:"log_id"`
	Revision                 int64   `json:"revision"`
	Generation               int64   `json:"generation"`
	PredecessorLogID         *string `json:"predecessor_log_id"`
	PredecessorReceiptSHA256 *string `json:"predecessor_receipt_sha256"`
	WithheldSelectionSHA256  string  `json:"withheld_selection_sha256"`
	PolicySHA256             string  `json:"policy_sha256"`
	PlayersSHA256            string  `json:"players_sha256"`
	ProducedSHA256           string  `json:"produced_sha256"`
	ProducedJSON             []byte  `json:"produced_json" format:"byte"`
	RowsSHA256               string  `json:"rows_sha256"`
}

type productionMetadataRowRow struct {
	LogID         string `json:"log_id"`
	Revision      int64  `json:"revision"`
	RowOrdinal    int64  `json:"row_ordinal"`
	RowID         string `json:"row_id"`
	CanonicalJSON []byte `json:"canonical_json" format:"byte"`
}

type productionMetadataValidationRow struct {
	LogID           string `json:"log_id"`
	Revision        int64  `json:"revision"`
	OperationID     string `json:"operation_id"`
	RequestSHA256   string `json:"request_sha256"`
	DraftGeneration int64  `json:"draft_generation"`
	InputsSHA256    string `json:"inputs_sha256"`
	RowsSHA256      string `json:"rows_sha256"`
	CanonicalJSON   []byte `json:"canonical_json" format:"byte"`
}

type productionMetadataBindingRow struct {
	LogID            string `json:"log_id"`
	Revision         int64  `json:"revision"`
	ApprovalID       string `json:"approval_id"`
	EvaluationSHA256 string `json:"evaluation_sha256"`
	CanonicalJSON    []byte `json:"canonical_json" format:"byte"`
}

type productionMetadataReceiptRow struct {
	LogID         string `json:"log_id"`
	Revision      int64  `json:"revision"`
	SHA256        string `json:"sha256"`
	CanonicalJSON []byte `json:"canonical_json" format:"byte"`
}

type productionMetadataAttachmentRow struct {
	AttachmentID              string `json:"attachment_id"`
	PrivilegeLogReceiptSHA256 string `json:"privilege_log_receipt_sha256"`
	SHA256                    string `json:"sha256"`
	CanonicalJSON             []byte `json:"canonical_json" format:"byte"`
}

type productionMetadataOperationRow struct {
	OperationID    string `json:"operation_id"`
	Kind           string `json:"kind"`
	RequestSHA256  string `json:"request_sha256"`
	ResponseSHA256 string `json:"response_sha256"`
	ResponseJSON   []byte `json:"response_json" format:"byte"`
}

func exportProductionMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	exports := []struct {
		kind, query string
		scan        func(*sql.Rows) (any, string, error)
	}{
		{productionMetadataPolicy, `SELECT policy_id,version,sha256,canonical_json FROM production_policy_versions ORDER BY policy_id,version`, func(rows *sql.Rows) (any, string, error) {
			var value productionMetadataPolicyRow
			err := rows.Scan(&value.PolicyID, &value.Version, &value.SHA256, &value.CanonicalJSON)
			return value, value.SHA256, err
		}},
		{productionMetadataApproval, `SELECT approval_id,subject_sha256,subject_json,authority_sha256,authority_json,grant_sha256,grant_json FROM production_approval_grants ORDER BY approval_id`, func(rows *sql.Rows) (any, string, error) {
			var value productionMetadataApprovalRow
			err := rows.Scan(&value.ApprovalID, &value.SubjectSHA256, &value.SubjectJSON, &value.AuthoritySHA256, &value.AuthorityJSON, &value.GrantSHA256, &value.GrantJSON)
			return value, value.ApprovalID, err
		}},
		{productionMetadataEvent, `SELECT event_id,approval_id,event_sha256,canonical_json FROM production_approval_events ORDER BY approval_id,event_id`, func(rows *sql.Rows) (any, string, error) {
			var value productionMetadataEventRow
			err := rows.Scan(&value.EventID, &value.ApprovalID, &value.EventSHA256, &value.CanonicalJSON)
			return value, value.EventID, err
		}},
		{productionMetadataPlayers, `SELECT snapshot_id,revision,sha256,canonical_json FROM production_players_snapshots ORDER BY snapshot_id,revision`, func(rows *sql.Rows) (any, string, error) {
			var value productionMetadataPlayersRow
			err := rows.Scan(&value.SnapshotID, &value.Revision, &value.SHA256, &value.CanonicalJSON)
			return value, value.SHA256, err
		}},
		{productionMetadataWithheld, `SELECT selection_id,set_id,revision,policy_sha256,sha256,canonical_json FROM production_withheld_selections ORDER BY selection_id`, func(rows *sql.Rows) (any, string, error) {
			var value productionMetadataWithheldRow
			err := rows.Scan(&value.SelectionID, &value.SetID, &value.Revision, &value.PolicySHA256, &value.SHA256, &value.CanonicalJSON)
			return value, value.SHA256, err
		}},
		{productionMetadataDraft, `SELECT log_id,revision,generation,predecessor_log_id,predecessor_receipt_sha256,withheld_selection_sha256,policy_sha256,players_sha256,produced_sha256,produced_json,rows_sha256 FROM production_privilege_log_drafts ORDER BY log_id,revision`, func(rows *sql.Rows) (any, string, error) {
			var value productionMetadataDraftRow
			err := rows.Scan(&value.LogID, &value.Revision, &value.Generation, &value.PredecessorLogID, &value.PredecessorReceiptSHA256, &value.WithheldSelectionSHA256, &value.PolicySHA256, &value.PlayersSHA256, &value.ProducedSHA256, &value.ProducedJSON, &value.RowsSHA256)
			return value, fmt.Sprintf("%s:%d", value.LogID, value.Revision), err
		}},
		{productionMetadataRow, `SELECT log_id,revision,row_ordinal,row_id,canonical_json FROM production_privilege_log_rows ORDER BY log_id,revision,row_ordinal`, func(rows *sql.Rows) (any, string, error) {
			var value productionMetadataRowRow
			err := rows.Scan(&value.LogID, &value.Revision, &value.RowOrdinal, &value.RowID, &value.CanonicalJSON)
			return value, fmt.Sprintf("%s:%d:%d", value.LogID, value.Revision, value.RowOrdinal), err
		}},
		{productionMetadataValidation, `SELECT log_id,revision,operation_id,request_sha256,draft_generation,inputs_sha256,rows_sha256,canonical_json FROM production_privilege_log_validations ORDER BY log_id,revision`, func(rows *sql.Rows) (any, string, error) {
			var value productionMetadataValidationRow
			err := rows.Scan(&value.LogID, &value.Revision, &value.OperationID, &value.RequestSHA256, &value.DraftGeneration, &value.InputsSHA256, &value.RowsSHA256, &value.CanonicalJSON)
			return value, fmt.Sprintf("%s:%d", value.LogID, value.Revision), err
		}},
		{productionMetadataBinding, `SELECT log_id,revision,approval_id,evaluation_sha256,canonical_json FROM production_privilege_log_approvals ORDER BY log_id,revision`, func(rows *sql.Rows) (any, string, error) {
			var value productionMetadataBindingRow
			err := rows.Scan(&value.LogID, &value.Revision, &value.ApprovalID, &value.EvaluationSHA256, &value.CanonicalJSON)
			return value, fmt.Sprintf("%s:%d", value.LogID, value.Revision), err
		}},
		{productionMetadataReceipt, `SELECT log_id,revision,sha256,canonical_json FROM production_privilege_log_receipts ORDER BY log_id,revision`, func(rows *sql.Rows) (any, string, error) {
			var value productionMetadataReceiptRow
			err := rows.Scan(&value.LogID, &value.Revision, &value.SHA256, &value.CanonicalJSON)
			return value, value.SHA256, err
		}},
		{productionMetadataAttachment, `SELECT attachment_id,privilege_log_receipt_sha256,sha256,canonical_json FROM production_privilege_log_attachments ORDER BY attachment_id`, func(rows *sql.Rows) (any, string, error) {
			var value productionMetadataAttachmentRow
			err := rows.Scan(&value.AttachmentID, &value.PrivilegeLogReceiptSHA256, &value.SHA256, &value.CanonicalJSON)
			return value, value.SHA256, err
		}},
		{productionMetadataOperation, `SELECT operation_id,kind,request_sha256,response_sha256,response_json FROM production_operation_receipts ORDER BY operation_id`, func(rows *sql.Rows) (any, string, error) {
			var value productionMetadataOperationRow
			err := rows.Scan(&value.OperationID, &value.Kind, &value.RequestSHA256, &value.ResponseSHA256, &value.ResponseJSON)
			return value, value.OperationID, err
		}},
	}
	for _, export := range exports {
		if err := func() error {
			rows, err := q.QueryContext(ctx, export.query)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				value, key, scanErr := export.scan(rows)
				if scanErr != nil {
					return scanErr
				}
				canonicalJSON, encodeErr := canonical.Marshal(value)
				if encodeErr != nil {
					return encodeErr
				}
				if writeErr := write(metadataProductionAuthority{Type: metadataProductionAuthorityType,
					Kind: export.kind, Key: key, CanonicalJSON: canonicalJSON,
					Checksum: digestProductionBytes(canonicalJSON)}); writeErr != nil {
					return writeErr
				}
			}
			return rows.Err()
		}(); err != nil {
			return err
		}
	}
	return nil
}

func importProductionMetadata(ctx context.Context, tx *sql.Tx, raw jsontext.Value) error {
	var record metadataProductionAuthority
	if err := decodeMetadataRecord(raw, &record); err != nil {
		return err
	}
	if record.Type != metadataProductionAuthorityType || digestProductionBytes(record.CanonicalJSON) != record.Checksum {
		return errors.New("invalid production metadata checksum")
	}
	decode := func(dst any) error {
		if err := json.Unmarshal(record.CanonicalJSON, dst, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		reencoded, err := canonical.Marshal(dst)
		if err != nil {
			return err
		}
		if !bytes.Equal(reencoded, record.CanonicalJSON) {
			return errors.New("production metadata row is not canonical")
		}
		return nil
	}
	switch record.Kind {
	case productionMetadataPolicy:
		var value productionMetadataPolicyRow
		if err := decode(&value); err != nil || record.Key != value.SHA256 {
			return errors.New("invalid production policy metadata key")
		}
		if _, err := decodeProductionPolicy(value.CanonicalJSON, value.SHA256); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO production_policy_versions(policy_id,version,sha256,canonical_json) VALUES(?,?,?,?)`, value.PolicyID, value.Version, value.SHA256, value.CanonicalJSON)
		return err
	case productionMetadataApproval:
		var value productionMetadataApprovalRow
		if err := decode(&value); err != nil || record.Key != value.ApprovalID {
			return errors.New("invalid production approval metadata key")
		}
		if _, err := decodeApprovalSubject(value.SubjectJSON, value.SubjectSHA256); err != nil {
			return err
		}
		if _, err := decodeApprovalAuthority(value.AuthorityJSON, value.AuthoritySHA256); err != nil {
			return err
		}
		if _, err := decodeApprovalGrant(value.GrantJSON, value.GrantSHA256); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO production_approval_grants(approval_id,subject_sha256,subject_json,authority_sha256,authority_json,grant_sha256,grant_json) VALUES(?,?,?,?,?,?,?)`, value.ApprovalID, value.SubjectSHA256, value.SubjectJSON, value.AuthoritySHA256, value.AuthorityJSON, value.GrantSHA256, value.GrantJSON)
		return err
	case productionMetadataEvent:
		var value productionMetadataEventRow
		if err := decode(&value); err != nil || record.Key != value.EventID || digestProductionBytes(value.CanonicalJSON) != value.EventSHA256 {
			return errors.New("invalid production approval event metadata")
		}
		event, err := canonical.Decode[documentproduction.ApprovalEvent](value.CanonicalJSON)
		if err != nil || event.ID != value.EventID || event.ApprovalID != value.ApprovalID {
			return errors.New("invalid production approval event relationship")
		}
		if _, _, err = documentproduction.CanonicalApprovalEvents([]documentproduction.ApprovalEvent{event}); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO production_approval_events(event_id,approval_id,event_sha256,canonical_json) VALUES(?,?,?,?)`, value.EventID, value.ApprovalID, value.EventSHA256, value.CanonicalJSON)
		return err
	case productionMetadataPlayers:
		var value productionMetadataPlayersRow
		if err := decode(&value); err != nil || record.Key != value.SHA256 {
			return errors.New("invalid production players metadata key")
		}
		if _, err := decodePlayersSnapshot(value.CanonicalJSON, value.SHA256); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO production_players_snapshots(snapshot_id,revision,sha256,canonical_json) VALUES(?,?,?,?)`, value.SnapshotID, value.Revision, value.SHA256, value.CanonicalJSON)
		return err
	case productionMetadataWithheld:
		var value productionMetadataWithheldRow
		if err := decode(&value); err != nil || record.Key != value.SHA256 {
			return errors.New("invalid production withheld metadata key")
		}
		if _, err := decodeWithheldSelection(value.CanonicalJSON, value.SHA256); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO production_withheld_selections(selection_id,set_id,revision,policy_sha256,sha256,canonical_json) VALUES(?,?,?,?,?,?)`, value.SelectionID, value.SetID, value.Revision, value.PolicySHA256, value.SHA256, value.CanonicalJSON)
		return err
	case productionMetadataDraft:
		var value productionMetadataDraftRow
		if err := decode(&value); err != nil || record.Key != fmt.Sprintf("%s:%d", value.LogID, value.Revision) {
			return errors.New("invalid production privilege draft metadata key")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO production_privilege_log_drafts(log_id,revision,generation,predecessor_log_id,predecessor_receipt_sha256,withheld_selection_sha256,policy_sha256,players_sha256,produced_sha256,produced_json,rows_sha256) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, value.LogID, value.Revision, value.Generation, value.PredecessorLogID, value.PredecessorReceiptSHA256, value.WithheldSelectionSHA256, value.PolicySHA256, value.PlayersSHA256, value.ProducedSHA256, value.ProducedJSON, value.RowsSHA256)
		return err
	case productionMetadataRow:
		var value productionMetadataRowRow
		if err := decode(&value); err != nil || record.Key != fmt.Sprintf("%s:%d:%d", value.LogID, value.Revision, value.RowOrdinal) {
			return errors.New("invalid production privilege row metadata key")
		}
		row, err := canonical.Decode[documentproduction.PrivilegeRow](value.CanonicalJSON)
		if err != nil || row.ID != value.RowID {
			return errors.New("invalid production privilege row")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO production_privilege_log_rows(log_id,revision,row_ordinal,row_id,canonical_json) VALUES(?,?,?,?,?)`, value.LogID, value.Revision, value.RowOrdinal, value.RowID, value.CanonicalJSON)
		return err
	case productionMetadataValidation:
		var value productionMetadataValidationRow
		if err := decode(&value); err != nil || record.Key != fmt.Sprintf("%s:%d", value.LogID, value.Revision) {
			return errors.New("invalid production privilege validation metadata key")
		}
		if _, err := canonical.Decode[productionservice.PreparedPrivilegeLogValidation](value.CanonicalJSON); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO production_privilege_log_validations(log_id,revision,operation_id,request_sha256,draft_generation,inputs_sha256,rows_sha256,canonical_json) VALUES(?,?,?,?,?,?,?,?)`, value.LogID, value.Revision, value.OperationID, value.RequestSHA256, value.DraftGeneration, value.InputsSHA256, value.RowsSHA256, value.CanonicalJSON)
		return err
	case productionMetadataBinding:
		var value productionMetadataBindingRow
		if err := decode(&value); err != nil || record.Key != fmt.Sprintf("%s:%d", value.LogID, value.Revision) {
			return errors.New("invalid production privilege approval metadata key")
		}
		if _, err := decodeApprovalEvaluation(value.CanonicalJSON, value.EvaluationSHA256); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO production_privilege_log_approvals(log_id,revision,approval_id,evaluation_sha256,canonical_json) VALUES(?,?,?,?,?)`, value.LogID, value.Revision, value.ApprovalID, value.EvaluationSHA256, value.CanonicalJSON)
		return err
	case productionMetadataReceipt:
		var value productionMetadataReceiptRow
		if err := decode(&value); err != nil || record.Key != value.SHA256 {
			return errors.New("invalid production privilege receipt metadata key")
		}
		if _, err := decodePrivilegeReceipt(value.CanonicalJSON, value.SHA256); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO production_privilege_log_receipts(log_id,revision,sha256,canonical_json) VALUES(?,?,?,?)`, value.LogID, value.Revision, value.SHA256, value.CanonicalJSON)
		return err
	case productionMetadataAttachment:
		var value productionMetadataAttachmentRow
		if err := decode(&value); err != nil || record.Key != value.SHA256 {
			return errors.New("invalid production privilege attachment metadata key")
		}
		var attachment documentproduction.PrivilegeLogAttachmentReceipt
		if err := json.Unmarshal(value.CanonicalJSON, &attachment, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		canonicalJSON, digest, err := documentproduction.CanonicalPrivilegeLogAttachment(attachment)
		if err != nil || digest != value.SHA256 || !bytes.Equal(canonicalJSON, value.CanonicalJSON) || attachment.PrivilegeLogReceiptSHA256 != value.PrivilegeLogReceiptSHA256 {
			return errors.New("invalid production privilege attachment")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO production_privilege_log_attachments(attachment_id,privilege_log_receipt_sha256,sha256,canonical_json) VALUES(?,?,?,?)`, value.AttachmentID, value.PrivilegeLogReceiptSHA256, value.SHA256, value.CanonicalJSON)
		return err
	case productionMetadataOperation:
		var value productionMetadataOperationRow
		if err := decode(&value); err != nil || record.Key != value.OperationID || digestProductionBytes(value.ResponseJSON) != value.ResponseSHA256 || !knownProductionOperation(value.Kind) {
			return errors.New("invalid production operation receipt")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO production_operation_receipts(operation_id,kind,request_sha256,response_sha256,response_json) VALUES(?,?,?,?,?)`, value.OperationID, value.Kind, value.RequestSHA256, value.ResponseSHA256, value.ResponseJSON)
		return err
	default:
		return fmt.Errorf("unknown production metadata kind %q", record.Kind)
	}
}

func validateProductionMetadataState(ctx context.Context, q metadataQuerier) error {
	if err := validateProductionCanonicalTables(ctx, q); err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, `SELECT log_id,revision,produced_sha256,produced_json,
		COALESCE(predecessor_log_id,''),COALESCE(predecessor_receipt_sha256,'')
		FROM production_privilege_log_drafts ORDER BY log_id,revision`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var logID, producedDigest, predecessorID, predecessorReceipt string
		var revision int64
		var producedJSON []byte
		if err := rows.Scan(&logID, &revision, &producedDigest, &producedJSON, &predecessorID, &predecessorReceipt); err != nil {
			_ = rows.Close()
			return err
		}
		if digestProductionBytes(producedJSON) != producedDigest {
			_ = rows.Close()
			return changedProductionPayload(logID)
		}
		produced, err := canonical.Decode[[]redaction.Member](producedJSON)
		if err != nil {
			_ = rows.Close()
			return err
		}
		for _, member := range produced {
			if err := redaction.ValidateMember(member); err != nil {
				_ = rows.Close()
				return err
			}
		}
		stored, err := loadStoredPrivilegeLog(ctx, q, logID, revision)
		if err != nil {
			_ = rows.Close()
			return err
		}
		if err := documentproduction.ValidateSelectionPartition(stored.Produced, stored.Withheld); err != nil {
			_ = rows.Close()
			return err
		}
		if predecessorID != "" {
			var actual string
			if err := q.QueryRowContext(ctx, `SELECT log_id FROM production_privilege_log_receipts WHERE sha256=?`, predecessorReceipt).Scan(&actual); err != nil || actual != predecessorID {
				_ = rows.Close()
				return errors.New("production privilege predecessor relationship is invalid")
			}
		}
		if stored.Withheld.PolicySHA256 != stored.Policy.SHA256 {
			_ = rows.Close()
			return errors.New("production privilege draft policy relationship is invalid")
		}
		if stored.Validation != nil {
			validatedAt, parseErr := time.Parse(time.RFC3339Nano, stored.Validation.Validation.Inputs.ValidatedAt)
			if parseErr != nil {
				_ = rows.Close()
				return fmt.Errorf("parsing stored production validation time: %w", parseErr)
			}
			current, validateErr := documentproduction.ValidatePrivilegeLog(documentproduction.PrivilegeLogValidationInput{
				LogID: stored.LogID, Revision: stored.Revision,
				WithheldSelectionSHA256: stored.Withheld.SHA256, PolicySHA256: stored.Policy.SHA256,
				PlayersSHA256: stored.Players.SHA256, ValidatedAt: validatedAt.Format(time.RFC3339Nano),
				Rows: stored.Rows,
			}, stored.Withheld, stored.Policy, stored.Players, stored.Produced)
			if validateErr != nil || stored.Validation.DraftGeneration != stored.Generation ||
				current.RowsSHA256 != stored.Validation.Validation.RowsSHA256 ||
				current.InputsSHA256 != stored.Validation.Validation.InputsSHA256 {
				_ = rows.Close()
				if validateErr != nil {
					return validateErr
				}
				return errors.New("production privilege validation is stale")
			}
		}
		if stored.ApprovalEvaluation != nil {
			if stored.Validation == nil || stored.ApprovalGrant == nil || stored.ApprovalSubject == nil ||
				!stored.Policy.Approval.Required ||
				stored.ApprovalEvaluation.ApprovalSHA256 != stored.ApprovalGrant.SHA256 ||
				stored.ApprovalEvaluation.SubjectSHA256 != stored.ApprovalGrant.SubjectSHA256 ||
				stored.ApprovalSubject.SetID != stored.Withheld.SetID ||
				stored.ApprovalSubject.Revision != stored.Withheld.Revision ||
				stored.ApprovalSubject.WithheldSelectionSHA256 != stored.Withheld.SHA256 ||
				stored.ApprovalSubject.PrivilegeLogInputsSHA256 != stored.Validation.Validation.InputsSHA256 {
				_ = rows.Close()
				return errors.New("production privilege approval binding is invalid")
			}
		} else if stored.ApprovalGrant != nil || stored.ApprovalSubject != nil || len(stored.ApprovalEvents) != 0 {
			_ = rows.Close()
			return errors.New("production privilege approval binding is incomplete")
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	return validateProductionReceiptsAndOperations(ctx, q)
}

func validateProductionCanonicalTables(ctx context.Context, q metadataQuerier) error {
	rows, err := q.QueryContext(ctx, `SELECT policy_id,version FROM production_policy_versions ORDER BY policy_id,version`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var version int64
		if err := rows.Scan(&id, &version); err != nil {
			_ = rows.Close()
			return err
		}
		var raw []byte
		var digest string
		if err := q.QueryRowContext(ctx, `SELECT canonical_json,sha256 FROM production_policy_versions WHERE policy_id=? AND version=?`, id, version).Scan(&raw, &digest); err != nil {
			_ = rows.Close()
			return err
		}
		if _, err := decodeProductionPolicy(raw, digest); err != nil {
			_ = rows.Close()
			return err
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	approvalRows, err := q.QueryContext(ctx, `SELECT approval_id FROM production_approval_grants ORDER BY approval_id`)
	if err != nil {
		return err
	}
	defer func() { _ = approvalRows.Close() }()
	for approvalRows.Next() {
		var id string
		if err := approvalRows.Scan(&id); err != nil {
			_ = approvalRows.Close()
			return err
		}
		grant, subject, authority, err := loadProductionApproval(ctx, q, id)
		if err != nil || grant.SubjectSHA256 == "" || authority.EvidenceSHA256 == "" || subject.Policy.PolicySHA256 == "" {
			_ = approvalRows.Close()
			if err != nil {
				return err
			}
			return errors.New("invalid production approval relationship")
		}
		_, subjectDigest, subjectErr := documentproduction.CanonicalApprovalSubject(subject)
		_, authorityDigest, authorityErr := documentproduction.CanonicalApprovalAuthority(authority)
		if subjectErr != nil || authorityErr != nil || grant.SubjectSHA256 != subjectDigest ||
			grant.AuthoritySHA256 != authorityDigest {
			_ = approvalRows.Close()
			return errors.New("invalid production approval canonical relationship")
		}
		if _, err = loadProductionPolicyByDigest(ctx, q, subject.Policy.PolicySHA256); err != nil {
			_ = approvalRows.Close()
			return err
		}
		if _, err = loadProductionApprovalEvents(ctx, q, id); err != nil {
			_ = approvalRows.Close()
			return err
		}
	}
	if err := errors.Join(approvalRows.Err(), approvalRows.Close()); err != nil {
		return err
	}
	playerRows, err := q.QueryContext(ctx, `SELECT canonical_json,sha256 FROM production_players_snapshots`)
	if err != nil {
		return err
	}
	defer func() { _ = playerRows.Close() }()
	for playerRows.Next() {
		var raw []byte
		var digest string
		if err := playerRows.Scan(&raw, &digest); err != nil {
			_ = playerRows.Close()
			return err
		}
		if _, err := decodePlayersSnapshot(raw, digest); err != nil {
			_ = playerRows.Close()
			return err
		}
	}
	if err := errors.Join(playerRows.Err(), playerRows.Close()); err != nil {
		return err
	}
	withheldRows, err := q.QueryContext(ctx, `SELECT canonical_json,sha256,policy_sha256 FROM production_withheld_selections`)
	if err != nil {
		return err
	}
	defer func() { _ = withheldRows.Close() }()
	for withheldRows.Next() {
		var raw []byte
		var digest, policyDigest string
		if err := withheldRows.Scan(&raw, &digest, &policyDigest); err != nil {
			_ = withheldRows.Close()
			return err
		}
		value, err := decodeWithheldSelection(raw, digest)
		if err != nil || value.PolicySHA256 != policyDigest {
			_ = withheldRows.Close()
			if err != nil {
				return err
			}
			return errors.New("invalid production withheld policy relationship")
		}
	}
	return errors.Join(withheldRows.Err(), withheldRows.Close())
}

func validateProductionReceiptsAndOperations(ctx context.Context, q metadataQuerier) error {
	receipts, err := q.QueryContext(ctx, `SELECT log_id,revision,canonical_json,sha256 FROM production_privilege_log_receipts`)
	if err != nil {
		return err
	}
	defer func() { _ = receipts.Close() }()
	for receipts.Next() {
		var logID string
		var revision int64
		var raw []byte
		var digest string
		if err := receipts.Scan(&logID, &revision, &raw, &digest); err != nil {
			_ = receipts.Close()
			return err
		}
		receipt, err := decodePrivilegeReceipt(raw, digest)
		if err != nil {
			_ = receipts.Close()
			return err
		}
		stored, err := loadStoredPrivilegeLog(ctx, q, logID, revision)
		if err != nil || stored.Validation == nil || receipt.RowCount != len(stored.Rows) ||
			receipt.RowsSHA256 != privilegeRowsDigest(stored.Rows) ||
			receipt.InputsSHA256 != stored.Validation.Validation.InputsSHA256 ||
			receipt.WithheldSelectionSHA256 != stored.Withheld.SHA256 || receipt.PolicySHA256 != stored.Policy.SHA256 ||
			receipt.PlayersSHA256 != stored.Players.SHA256 {
			_ = receipts.Close()
			if err != nil {
				return err
			}
			return errors.New("production privilege receipt relationship is invalid")
		}
		if stored.ApprovalEvaluation == nil && receipt.ApprovalEvaluationSHA256 != "" ||
			stored.ApprovalEvaluation != nil && receipt.ApprovalEvaluationSHA256 != stored.ApprovalEvaluation.SHA256 {
			_ = receipts.Close()
			return errors.New("production privilege receipt approval relationship is invalid")
		}
	}
	if err := errors.Join(receipts.Err(), receipts.Close()); err != nil {
		return err
	}
	attachments, err := q.QueryContext(ctx, `SELECT canonical_json,sha256,privilege_log_receipt_sha256 FROM production_privilege_log_attachments`)
	if err != nil {
		return err
	}
	defer func() { _ = attachments.Close() }()
	for attachments.Next() {
		var raw []byte
		var digest, receiptDigest string
		if err := attachments.Scan(&raw, &digest, &receiptDigest); err != nil {
			_ = attachments.Close()
			return err
		}
		var value documentproduction.PrivilegeLogAttachmentReceipt
		if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
			_ = attachments.Close()
			return err
		}
		canonicalJSON, actual, err := documentproduction.CanonicalPrivilegeLogAttachment(value)
		if err != nil || actual != digest || !bytes.Equal(canonicalJSON, raw) || value.PrivilegeLogReceiptSHA256 != receiptDigest {
			_ = attachments.Close()
			return errors.New("production privilege attachment relationship is invalid")
		}
	}
	if err := errors.Join(attachments.Err(), attachments.Close()); err != nil {
		return err
	}
	operations, err := q.QueryContext(ctx, `SELECT operation_id,kind,request_sha256,response_sha256,response_json FROM production_operation_receipts`)
	if err != nil {
		return err
	}
	defer func() { _ = operations.Close() }()
	for operations.Next() {
		var id, kind, requestDigest, responseDigest string
		var response []byte
		if err := operations.Scan(&id, &kind, &requestDigest, &responseDigest, &response); err != nil {
			_ = operations.Close()
			return err
		}
		if validateUUIDv4(id) != nil || !canonical.IsSHA256Hex(requestDigest) || !knownProductionOperation(kind) || digestProductionBytes(response) != responseDigest {
			_ = operations.Close()
			return errors.New("invalid production operation receipt")
		}
		if err := validateProductionOperationResponse(ctx, q, id, kind, response); err != nil {
			_ = operations.Close()
			return err
		}
	}
	return errors.Join(operations.Err(), operations.Close())
}

func validateProductionOperationResponse(ctx context.Context, q metadataQuerier, operationID, kind string, raw []byte) error {
	switch kind {
	case productionOperationPolicy:
		value, err := canonical.Decode[documentproduction.PolicyVersion](raw)
		if err != nil {
			return err
		}
		stored, err := loadProductionPolicyByDigest(ctx, q, value.SHA256)
		if err != nil || stored.ID != value.ID || stored.Version != value.Version {
			return errors.New("production policy operation receipt is detached")
		}
	case productionOperationApproval:
		value, err := canonical.Decode[documentproduction.ApprovalGrant](raw)
		if err != nil {
			return err
		}
		stored, _, _, err := loadProductionApproval(ctx, q, value.ID)
		if err != nil || stored.SHA256 != value.SHA256 {
			return errors.New("production approval operation receipt is detached")
		}
	case productionOperationApprovalEvent:
		value, err := canonical.Decode[documentproduction.ApprovalEvent](raw)
		if err != nil {
			return err
		}
		var storedApproval string
		if err := q.QueryRowContext(ctx, `SELECT approval_id FROM production_approval_events WHERE event_id=?`, value.ID).Scan(&storedApproval); err != nil || storedApproval != value.ApprovalID {
			return errors.New("production approval event operation receipt is detached")
		}
	case productionOperationPlayers:
		value, err := canonical.Decode[documentproduction.PlayersSnapshot](raw)
		if err != nil {
			return err
		}
		if _, err := loadProductionPlayers(ctx, q, value.SHA256); err != nil {
			return errors.New("production players operation receipt is detached")
		}
	case productionOperationWithheld:
		value, err := canonical.Decode[documentproduction.WithheldSelection](raw)
		if err != nil {
			return err
		}
		if _, err := loadProductionWithheld(ctx, q, value.SHA256); err != nil {
			return errors.New("production withheld operation receipt is detached")
		}
	case productionOperationValidation:
		value, err := canonical.Decode[productionservice.PreparedPrivilegeLogValidation](raw)
		if err != nil || value.OperationID != operationID {
			return errors.New("production validation operation receipt is detached")
		}
	case productionOperationPreparedInputs:
		value, err := canonical.Decode[documentproduction.PreparedInputAuthority](raw)
		if err != nil || documentproduction.ValidatePreparedInputAuthority(value) != nil || value.Audit.OperationID != operationID {
			return errors.New("production prepared-input operation receipt is detached")
		}
	case productionOperationFreeze:
		value, err := canonical.Decode[documentproduction.PrivilegeLogReceipt](raw)
		if err != nil {
			return err
		}
		var exists bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM production_privilege_log_receipts WHERE sha256=?)`, value.SHA256).Scan(&exists); err != nil || !exists {
			return errors.New("production freeze operation receipt is detached")
		}
	case productionOperationAttachment:
		value, err := canonical.Decode[documentproduction.PrivilegeLogAttachmentReceipt](raw)
		if err != nil {
			return err
		}
		var exists bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM production_privilege_log_attachments WHERE sha256=?)`, value.SHA256).Scan(&exists); err != nil || !exists {
			return errors.New("production attachment operation receipt is detached")
		}
	case productionOperationDraft, productionOperationRows, productionOperationApprovalBind:
		if _, err := canonical.Decode[productionGenerationReceipt](raw); err != nil {
			return err
		}
	default:
		return errors.New("unknown production operation receipt")
	}
	return nil
}

func knownProductionOperation(kind string) bool {
	switch kind {
	case productionOperationPolicy, productionOperationApproval, productionOperationApprovalEvent,
		productionOperationPlayers, productionOperationWithheld, productionOperationDraft,
		productionOperationRows, productionOperationValidation, productionOperationApprovalBind,
		productionOperationPreparedInputs, productionOperationFreeze, productionOperationAttachment:
		return true
	default:
		return false
	}
}
