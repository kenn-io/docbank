package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	metadataMediaSourceType             = "media_source"
	metadataMediaSourceVersionType      = "media_source_version"
	metadataMediaSourceHeadType         = "media_source_head"
	metadataMediaOccurrenceType         = "media_occurrence"
	metadataMediaVisibilityFenceType    = "media_visibility_fence"
	metadataMediaInputArtifactType      = "media_input_artifact"
	metadataMediaOperationType          = "media_operation"
	metadataMediaAcquisitionReceiptType = "media_acquisition_receipt"
)

var mediaMetadataRequiredFields = map[string][]string{
	metadataMediaSourceType:             {metadataTypeField, "source_id", "kind", "provider", "origin_scope", "identity_sha256", "created_at"},
	metadataMediaSourceVersionType:      {metadataTypeField, auditSourceVersionIDField, "source_id", "revision", "content_version_id", "source_sha256", "source_bytes", "capture_json", "claim_sha256", "created_at"},
	metadataMediaSourceHeadType:         {metadataTypeField, "source_id", auditSourceVersionIDField, "revision"},
	metadataMediaOccurrenceType:         {metadataTypeField, "occurrence_id", "source_id", auditSourceVersionIDField, "caller_principal", "caller_occurrence_ref", "caller_revision", "caller_filename", "caller_person_ref", "speaker_label", "message_json", "visible", "first_seen_at", "revoked_at"},
	metadataMediaVisibilityFenceType:    {metadataTypeField, "caller_principal", "fence", "updated_at"},
	metadataMediaInputArtifactType:      {metadataTypeField, "input_id", "occurrence_id", "source_id", auditSourceVersionIDField, "content_version_id", "kind", "origin", "provider", "language", "input_sha256", "created_at"},
	metadataMediaOperationType:          {metadataTypeField, "operation_id", "principal", "verb", "request_sha256", "state", "source_id", "receipt_json", "created_at", "updated_at"},
	metadataMediaAcquisitionReceiptType: {metadataTypeField, "acquisition_id", "operation_id", "occurrence_id", "source_id", "origin_id", "resolver_fingerprint", "request_sha256", "state", "stage", "outcome", "failure_code", "attempt", "claim_epoch", "available_at", "received_bytes", "started_at", "finished_at"},
}

var mediaMetadataNullableFields = map[string]map[string]bool{
	metadataMediaSourceHeadType:         {auditSourceVersionIDField: true},
	metadataMediaOccurrenceType:         {auditSourceVersionIDField: true, "revoked_at": true},
	metadataMediaInputArtifactType:      {auditSourceVersionIDField: true},
	metadataMediaOperationType:          {"source_id": true},
	metadataMediaAcquisitionReceiptType: {"started_at": true, "finished_at": true},
}

func init() {
	maps.Copy(metadataRequiredFields, mediaMetadataRequiredFields)
	maps.Copy(metadataNullableFields, mediaMetadataNullableFields)
}

type metadataMediaSource struct {
	Type           string `json:"type"`
	SourceID       string `json:"source_id"`
	Kind           string `json:"kind"`
	Provider       string `json:"provider"`
	OriginScope    string `json:"origin_scope"`
	IdentitySHA256 string `json:"identity_sha256"`
	CreatedAt      string `json:"created_at"`
}

type metadataMediaSourceVersion struct {
	Type             string `json:"type"`
	SourceVersionID  string `json:"source_version_id"`
	SourceID         string `json:"source_id"`
	Revision         int64  `json:"revision"`
	ContentVersionID string `json:"content_version_id"`
	SourceSHA256     string `json:"source_sha256"`
	SourceBytes      int64  `json:"source_bytes"`
	CaptureJSON      string `json:"capture_json"`
	ClaimSHA256      string `json:"claim_sha256"`
	CreatedAt        string `json:"created_at"`
}

type metadataMediaSourceHead struct {
	Type            string  `json:"type"`
	SourceID        string  `json:"source_id"`
	SourceVersionID *string `json:"source_version_id"`
	Revision        int64   `json:"revision"`
}

type metadataMediaOccurrence struct {
	Type                string  `json:"type"`
	OccurrenceID        string  `json:"occurrence_id"`
	SourceID            string  `json:"source_id"`
	SourceVersionID     *string `json:"source_version_id"`
	CallerPrincipal     string  `json:"caller_principal"`
	CallerOccurrenceRef string  `json:"caller_occurrence_ref"`
	CallerRevision      string  `json:"caller_revision"`
	CallerFilename      string  `json:"caller_filename"`
	CallerPersonRef     string  `json:"caller_person_ref"`
	SpeakerLabel        string  `json:"speaker_label"`
	MessageJSON         string  `json:"message_json"`
	Visible             int64   `json:"visible"`
	FirstSeenAt         string  `json:"first_seen_at"`
	RevokedAt           *string `json:"revoked_at"`
}

type metadataMediaVisibilityFence struct {
	Type            string `json:"type"`
	CallerPrincipal string `json:"caller_principal"`
	Fence           int64  `json:"fence"`
	UpdatedAt       string `json:"updated_at"`
}

type metadataMediaInputArtifact struct {
	Type             string  `json:"type"`
	InputID          string  `json:"input_id"`
	OccurrenceID     string  `json:"occurrence_id"`
	SourceID         string  `json:"source_id"`
	SourceVersionID  *string `json:"source_version_id"`
	ContentVersionID string  `json:"content_version_id"`
	Kind             string  `json:"kind"`
	Origin           string  `json:"origin"`
	Provider         string  `json:"provider"`
	Language         string  `json:"language"`
	InputSHA256      string  `json:"input_sha256"`
	CreatedAt        string  `json:"created_at"`
}

type metadataMediaOperation struct {
	Type          string  `json:"type"`
	OperationID   string  `json:"operation_id"`
	Principal     string  `json:"principal"`
	Verb          string  `json:"verb"`
	RequestSHA256 string  `json:"request_sha256"`
	State         string  `json:"state"`
	SourceID      *string `json:"source_id"`
	ReceiptJSON   string  `json:"receipt_json"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

type metadataMediaAcquisitionReceipt struct {
	Type                string  `json:"type"`
	AcquisitionID       string  `json:"acquisition_id"`
	OperationID         string  `json:"operation_id"`
	OccurrenceID        string  `json:"occurrence_id"`
	SourceID            string  `json:"source_id"`
	OriginID            string  `json:"origin_id"`
	ResolverFingerprint string  `json:"resolver_fingerprint"`
	RequestSHA256       string  `json:"request_sha256"`
	State               string  `json:"state"`
	Stage               string  `json:"stage"`
	Outcome             string  `json:"outcome"`
	FailureCode         string  `json:"failure_code"`
	Attempt             int64   `json:"attempt"`
	ClaimEpoch          int64   `json:"claim_epoch"`
	AvailableAt         string  `json:"available_at"`
	ReceivedBytes       int64   `json:"received_bytes"`
	StartedAt           *string `json:"started_at"`
	FinishedAt          *string `json:"finished_at"`
}

func exportMediaMetadata(ctx context.Context, query metadataQuerier, write metadataWrite) error {
	for _, exporter := range []func(context.Context, metadataQuerier, metadataWrite) error{
		exportMediaSources, exportMediaSourceVersions, exportMediaSourceHeads, exportMediaOccurrences,
		exportMediaVisibilityFences, exportMediaInputArtifacts, exportMediaOperations, exportMediaAcquisitionReceipts,
	} {
		if err := exporter(ctx, query, write); err != nil {
			return err
		}
	}
	return nil
}

func exportMediaSources(ctx context.Context, query metadataQuerier, write metadataWrite) error {
	return exportMediaRows(ctx, query, write, metadataMediaSourceType,
		`SELECT source_id,kind,provider,origin_scope,identity_sha256,created_at FROM media_sources ORDER BY source_id`,
		func(rows *sql.Rows) (any, error) {
			record := metadataMediaSource{Type: metadataMediaSourceType}
			err := rows.Scan(&record.SourceID, &record.Kind, &record.Provider, &record.OriginScope, &record.IdentitySHA256, &record.CreatedAt)
			if err == nil {
				err = validateMetadataMediaSource(record)
			}
			return record, err
		})
}

func exportMediaSourceVersions(ctx context.Context, query metadataQuerier, write metadataWrite) error {
	return exportMediaRows(ctx, query, write, metadataMediaSourceVersionType,
		`SELECT source_version_id,source_id,revision,content_version_id,source_sha256,source_bytes,capture_json,claim_sha256,created_at FROM media_source_versions ORDER BY source_version_id`,
		func(rows *sql.Rows) (any, error) {
			record := metadataMediaSourceVersion{Type: metadataMediaSourceVersionType}
			err := rows.Scan(&record.SourceVersionID, &record.SourceID, &record.Revision, &record.ContentVersionID, &record.SourceSHA256, &record.SourceBytes, &record.CaptureJSON, &record.ClaimSHA256, &record.CreatedAt)
			if err == nil {
				err = validateMetadataMediaSourceVersion(record)
			}
			return record, err
		})
}

func exportMediaSourceHeads(ctx context.Context, query metadataQuerier, write metadataWrite) error {
	return exportMediaRows(ctx, query, write, metadataMediaSourceHeadType,
		`SELECT source_id,source_version_id,revision FROM media_source_heads ORDER BY source_id`,
		func(rows *sql.Rows) (any, error) {
			record := metadataMediaSourceHead{Type: metadataMediaSourceHeadType}
			err := rows.Scan(&record.SourceID, &record.SourceVersionID, &record.Revision)
			if err == nil {
				err = validateMetadataMediaSourceHead(record)
			}
			return record, err
		})
}

func exportMediaOccurrences(ctx context.Context, query metadataQuerier, write metadataWrite) error {
	return exportMediaRows(ctx, query, write, metadataMediaOccurrenceType,
		`SELECT occurrence_id,source_id,source_version_id,caller_principal,caller_occurrence_ref,caller_revision,caller_filename,caller_person_ref,speaker_label,message_json,visible,first_seen_at,revoked_at FROM media_occurrences ORDER BY occurrence_id`,
		func(rows *sql.Rows) (any, error) {
			record := metadataMediaOccurrence{Type: metadataMediaOccurrenceType}
			err := rows.Scan(&record.OccurrenceID, &record.SourceID, &record.SourceVersionID, &record.CallerPrincipal, &record.CallerOccurrenceRef, &record.CallerRevision, &record.CallerFilename, &record.CallerPersonRef, &record.SpeakerLabel, &record.MessageJSON, &record.Visible, &record.FirstSeenAt, &record.RevokedAt)
			if err == nil {
				err = validateMetadataMediaOccurrence(record)
			}
			return record, err
		})
}

func exportMediaVisibilityFences(ctx context.Context, query metadataQuerier, write metadataWrite) error {
	return exportMediaRows(ctx, query, write, metadataMediaVisibilityFenceType,
		`SELECT caller_principal,fence,updated_at FROM media_visibility_fences ORDER BY caller_principal`,
		func(rows *sql.Rows) (any, error) {
			record := metadataMediaVisibilityFence{Type: metadataMediaVisibilityFenceType}
			err := rows.Scan(&record.CallerPrincipal, &record.Fence, &record.UpdatedAt)
			if err == nil {
				err = validateMetadataMediaVisibilityFence(record)
			}
			return record, err
		})
}

func exportMediaInputArtifacts(ctx context.Context, query metadataQuerier, write metadataWrite) error {
	return exportMediaRows(ctx, query, write, metadataMediaInputArtifactType,
		`SELECT input_id,occurrence_id,source_id,source_version_id,content_version_id,kind,origin,provider,language,input_sha256,created_at FROM media_input_artifacts ORDER BY input_id`,
		func(rows *sql.Rows) (any, error) {
			record := metadataMediaInputArtifact{Type: metadataMediaInputArtifactType}
			err := rows.Scan(&record.InputID, &record.OccurrenceID, &record.SourceID, &record.SourceVersionID, &record.ContentVersionID, &record.Kind, &record.Origin, &record.Provider, &record.Language, &record.InputSHA256, &record.CreatedAt)
			if err == nil {
				err = validateMetadataMediaInputArtifact(record)
			}
			return record, err
		})
}

func exportMediaOperations(ctx context.Context, query metadataQuerier, write metadataWrite) error {
	return exportMediaRows(ctx, query, write, metadataMediaOperationType,
		`SELECT operation_id,principal,verb,request_sha256,state,source_id,receipt_json,created_at,updated_at FROM media_operations ORDER BY operation_id`,
		func(rows *sql.Rows) (any, error) {
			record := metadataMediaOperation{Type: metadataMediaOperationType}
			err := rows.Scan(&record.OperationID, &record.Principal, &record.Verb, &record.RequestSHA256, &record.State, &record.SourceID, &record.ReceiptJSON, &record.CreatedAt, &record.UpdatedAt)
			if err == nil {
				err = validateMetadataMediaOperation(record)
			}
			return record, err
		})
}

func exportMediaAcquisitionReceipts(ctx context.Context, query metadataQuerier, write metadataWrite) error {
	return exportMediaRows(ctx, query, write, metadataMediaAcquisitionReceiptType,
		`SELECT acquisition_id,operation_id,occurrence_id,source_id,origin_id,resolver_fingerprint,request_sha256,state,stage,outcome,failure_code,attempt,claim_epoch,available_at,received_bytes,started_at,finished_at FROM media_acquisitions ORDER BY acquisition_id`,
		func(rows *sql.Rows) (any, error) {
			record := metadataMediaAcquisitionReceipt{Type: metadataMediaAcquisitionReceiptType}
			err := rows.Scan(&record.AcquisitionID, &record.OperationID, &record.OccurrenceID, &record.SourceID, &record.OriginID, &record.ResolverFingerprint, &record.RequestSHA256, &record.State, &record.Stage, &record.Outcome, &record.FailureCode, &record.Attempt, &record.ClaimEpoch, &record.AvailableAt, &record.ReceivedBytes, &record.StartedAt, &record.FinishedAt)
			if err == nil {
				err = validateMetadataMediaAcquisitionReceipt(record)
			}
			return record, err
		})
}

func exportMediaRows(ctx context.Context, query metadataQuerier, write metadataWrite, kind, statement string, scan func(*sql.Rows) (any, error)) error {
	rows, err := query.QueryContext(ctx, statement)
	if err != nil {
		return fmt.Errorf("exporting %s metadata: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record, err := scan(rows)
		if err != nil {
			return fmt.Errorf("validating %s metadata for export: %w", kind, err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError(kind, rows)
}

func isMediaMetadataType(kind string) bool {
	_, ok := mediaMetadataRequiredFields[kind]
	return ok
}

func (s *Store) importMediaMetadataRecord(ctx context.Context, tx *sql.Tx, kind string, raw jsontext.Value) error {
	switch kind {
	case metadataMediaSourceType:
		var record metadataMediaSource
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if err := validateMetadataMediaSource(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO media_sources VALUES(?,?,?,?,?,?)`, record.SourceID, record.Kind, record.Provider, record.OriginScope, record.IdentitySHA256, record.CreatedAt)
		return err
	case metadataMediaSourceVersionType:
		var record metadataMediaSourceVersion
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if err := validateMetadataMediaSourceVersion(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO media_source_versions VALUES(?,?,?,?,?,?,?,?,?)`, record.SourceVersionID, record.SourceID, record.Revision, record.ContentVersionID, record.SourceSHA256, record.SourceBytes, record.CaptureJSON, record.ClaimSHA256, record.CreatedAt)
		return err
	case metadataMediaSourceHeadType:
		var record metadataMediaSourceHead
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if err := validateMetadataMediaSourceHead(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO media_source_heads VALUES(?,?,?)`, record.SourceID, record.SourceVersionID, record.Revision)
		return err
	case metadataMediaOccurrenceType:
		var record metadataMediaOccurrence
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if err := validateMetadataMediaOccurrence(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO media_occurrences VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.OccurrenceID, record.SourceID, record.SourceVersionID, record.CallerPrincipal, record.CallerOccurrenceRef, record.CallerRevision, record.CallerFilename, record.CallerPersonRef, record.SpeakerLabel, record.MessageJSON, record.Visible, record.FirstSeenAt, record.RevokedAt)
		return err
	case metadataMediaVisibilityFenceType:
		var record metadataMediaVisibilityFence
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if err := validateMetadataMediaVisibilityFence(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO media_visibility_fences VALUES(?,?,?)`, record.CallerPrincipal, record.Fence, record.UpdatedAt)
		return err
	case metadataMediaInputArtifactType:
		var record metadataMediaInputArtifact
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if err := validateMetadataMediaInputArtifact(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO media_input_artifacts VALUES(?,?,?,?,?,?,?,?,?,?,?)`, record.InputID, record.OccurrenceID, record.SourceID, record.SourceVersionID, record.ContentVersionID, record.Kind, record.Origin, record.Provider, record.Language, record.InputSHA256, record.CreatedAt)
		return err
	case metadataMediaOperationType:
		var record metadataMediaOperation
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if err := validateMetadataMediaOperation(record); err != nil {
			return err
		}
		state := record.State
		if state == mediaOperationQueued || state == mediaOperationRunning {
			state = mediaOperationFailed
			if record.Verb == "submit_supplied_media" || record.Verb == "retry_media" {
				receipt, err := canonical.Decode[MediaPublicationReceipt]([]byte(record.ReceiptJSON))
				if err != nil {
					return err
				}
				receipt.OperationState, receipt.CoverageState, receipt.JobID = state, mediaCoverageUnavailable, ""
				encoded, err := canonical.Marshal(receipt)
				if err != nil {
					return err
				}
				record.ReceiptJSON = string(encoded)
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO media_operations VALUES(?,?,?,?,?,?,?,?,?)`, record.OperationID, record.Principal, record.Verb, record.RequestSHA256, state, record.SourceID, record.ReceiptJSON, record.CreatedAt, record.UpdatedAt)
		return err
	case metadataMediaAcquisitionReceiptType:
		var record metadataMediaAcquisitionReceipt
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if err := validateMetadataMediaAcquisitionReceipt(record); err != nil {
			return err
		}
		state, stage, outcome, failure, finished := record.State, record.Stage, record.Outcome, record.FailureCode, record.FinishedAt
		if state == mediaOperationQueued || state == mediaOperationRunning {
			state, stage, outcome, failure = mediaOperationFailed, "authorization", "access_required", "consent_absent"
			finished = &record.AvailableAt
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO media_acquisitions(
			acquisition_id,operation_id,occurrence_id,source_id,origin_id,resolver_fingerprint,
			request_sha256,authorization_json,state,stage,outcome,failure_code,attempt,claim_owner,
			claim_epoch,lease_expires_at,available_at,received_bytes,started_at,finished_at
		) VALUES(?,?,?,?,?,?,?,'{}',?,?,?,?,?,NULL,?,NULL,?,?,?,?)`, record.AcquisitionID,
			record.OperationID, record.OccurrenceID, record.SourceID, record.OriginID,
			record.ResolverFingerprint, record.RequestSHA256, state, stage, outcome, failure,
			record.Attempt, record.ClaimEpoch, record.AvailableAt, record.ReceivedBytes, record.StartedAt, finished)
		return err
	default:
		return fmt.Errorf("unknown media metadata type %q", kind)
	}
}

func validateMetadataMediaSource(record metadataMediaSource) error {
	if record.Type != metadataMediaSourceType {
		return errors.New("invalid media source record")
	}
	if err := validateBoundedMediaText("media source ID", record.SourceID, 256, false); err != nil {
		return err
	}
	if record.Kind != "supplied_media" && record.Kind != "remote_recording" {
		return errors.New("invalid media source kind")
	}
	if record.Kind == "supplied_media" && (record.Provider != "" || record.OriginScope != "") {
		return errors.New("supplied media source has remote identity fields")
	}
	if record.Kind == "remote_recording" {
		if err := validateBoundedMediaText("media source provider", record.Provider, 128, false); err != nil {
			return err
		}
		if err := validateBoundedMediaText("media source origin scope", record.OriginScope, 256, false); err != nil {
			return err
		}
	}
	if !canonical.IsSHA256Hex(record.IdentitySHA256) {
		return errors.New("invalid media source identity digest")
	}
	return validateMetadataTime("media source created_at", record.CreatedAt)
}

func validateMetadataMediaSourceVersion(record metadataMediaSourceVersion) error {
	if record.Type != metadataMediaSourceVersionType || record.Revision < 1 || record.SourceBytes < 0 {
		return errors.New("invalid media source version record")
	}
	for _, value := range []string{record.SourceVersionID, record.SourceID, record.ContentVersionID} {
		if err := validateBoundedMediaText("media source version identity", value, 256, false); err != nil {
			return err
		}
	}
	if !canonical.IsSHA256Hex(record.SourceSHA256) || !canonical.IsSHA256Hex(record.ClaimSHA256) {
		return errors.New("invalid media source version digest")
	}
	claim, err := canonicalJSONText(record.CaptureJSON, "media capture claim")
	if err != nil || digestCatalogJSON(claim) != record.ClaimSHA256 {
		return errors.New("invalid media capture claim")
	}
	return validateMetadataTime("media source version created_at", record.CreatedAt)
}

func validateMetadataMediaSourceHead(record metadataMediaSourceHead) error {
	if record.Type != metadataMediaSourceHeadType || record.Revision < 0 {
		return errors.New("invalid media source head record")
	}
	if err := validateBoundedMediaText("media source head source", record.SourceID, 256, false); err != nil {
		return err
	}
	if record.SourceVersionID == nil {
		if record.Revision != 0 {
			return errors.New("empty media source head has a revision")
		}
		return nil
	}
	if record.Revision < 1 {
		return errors.New("media source head lacks a revision")
	}
	return validateBoundedMediaText("media source head version", *record.SourceVersionID, 256, false)
}

func validateMetadataMediaOccurrence(record metadataMediaOccurrence) error {
	if record.Type != metadataMediaOccurrenceType || (record.Visible != 0 && record.Visible != 1) {
		return errors.New("invalid media occurrence record")
	}
	in := MediaOccurrenceInput{ID: record.OccurrenceID, SourceID: record.SourceID, Principal: record.CallerPrincipal, Ref: record.CallerOccurrenceRef, Revision: record.CallerRevision, Filename: record.CallerFilename, PersonRef: record.CallerPersonRef, SpeakerLabel: record.SpeakerLabel, MessageJSON: record.MessageJSON}
	if record.SourceVersionID != nil {
		in.SourceVersionID = *record.SourceVersionID
	}
	if err := validateMediaOccurrenceInput(in); err != nil {
		return err
	}
	if err := validateMetadataTime("media occurrence first_seen_at", record.FirstSeenAt); err != nil {
		return err
	}
	if record.RevokedAt != nil {
		if err := validateMetadataTime("media occurrence revoked_at", *record.RevokedAt); err != nil {
			return err
		}
	}
	if record.Visible == 1 && record.RevokedAt != nil {
		return errors.New("visible media occurrence is revoked")
	}
	if record.Visible == 0 && record.RevokedAt == nil {
		return errors.New("hidden media occurrence lacks revocation time")
	}
	return nil
}

func validateMetadataMediaVisibilityFence(record metadataMediaVisibilityFence) error {
	if record.Type != metadataMediaVisibilityFenceType || record.Fence < 1 {
		return errors.New("invalid media visibility fence record")
	}
	if err := validateBoundedMediaText("media visibility principal", record.CallerPrincipal, 256, false); err != nil {
		return err
	}
	return validateMetadataTime("media visibility fence updated_at", record.UpdatedAt)
}

func validateMetadataMediaInputArtifact(record metadataMediaInputArtifact) error {
	if record.Type != metadataMediaInputArtifactType {
		return errors.New("invalid media input artifact record")
	}
	for _, field := range []struct {
		name, value string
		maxBytes    int
	}{
		{"media input ID", record.InputID, 256}, {"media input occurrence", record.OccurrenceID, 256},
		{"media input source", record.SourceID, 256}, {"media input content version", record.ContentVersionID, 256},
	} {
		if err := validateBoundedMediaText(field.name, field.value, field.maxBytes, false); err != nil {
			return err
		}
	}
	if err := ValidateMediaInputAuthority(record.Kind, record.Origin, record.Provider,
		record.Language, record.InputSHA256); err != nil {
		return err
	}
	if record.SourceVersionID != nil {
		if err := validateBoundedMediaText("media input source version", *record.SourceVersionID, 256, false); err != nil {
			return err
		}
	}
	return validateMetadataTime("media input created_at", record.CreatedAt)
}

// ValidateMediaInputAuthority is the shared write/export validator for the
// portable labels and exact digest of a retained original input.
func ValidateMediaInputAuthority(kind, origin, provider, language, inputSHA256 string) error {
	if kind != "media" && kind != "caption" && kind != "transcript" {
		return errors.New("invalid media input kind")
	}
	for _, field := range []struct {
		name, value string
		maxBytes    int
		allowEmpty  bool
	}{
		{"media input kind", kind, 64, false}, {"media input origin", origin, 64, false},
		{"media input provider", provider, 128, true}, {"media input language", language, 128, true},
	} {
		if err := validateBoundedMediaText(field.name, field.value, field.maxBytes, field.allowEmpty); err != nil {
			return err
		}
	}
	if !canonical.IsSHA256Hex(inputSHA256) {
		return errors.New("invalid media input digest")
	}
	return nil
}

func validateMetadataMediaOperation(record metadataMediaOperation) error {
	if record.Type != metadataMediaOperationType || !validMediaOperationState(record.State) {
		return errors.New("invalid media operation record")
	}
	if err := validateUUIDv4(record.OperationID); err != nil {
		return err
	}
	if err := validateBoundedMediaText("media operation principal", record.Principal, 256, false); err != nil {
		return err
	}
	if !validMediaOperationVerb(record.Verb) || !canonical.IsSHA256Hex(record.RequestSHA256) {
		return ErrMediaOperationConflict
	}
	if record.SourceID != nil {
		if err := validateBoundedMediaText("media operation source", *record.SourceID, 256, false); err != nil {
			return err
		}
	}
	if err := validateMediaReceipt(record.ReceiptJSON); err != nil {
		return err
	}
	if err := validateMetadataTime("media operation created_at", record.CreatedAt); err != nil {
		return err
	}
	return validateMetadataTime("media operation updated_at", record.UpdatedAt)
}

func validateMetadataMediaAcquisitionReceipt(record metadataMediaAcquisitionReceipt) error {
	if record.Type != metadataMediaAcquisitionReceiptType || !validMediaOperationState(record.State) || record.Attempt < 0 || record.ClaimEpoch < 0 || record.ReceivedBytes < 0 {
		return errors.New("invalid media acquisition receipt")
	}
	for _, field := range []struct {
		name, value string
		maxBytes    int
	}{
		{"media acquisition ID", record.AcquisitionID, 256}, {"media acquisition operation", record.OperationID, 256},
		{"media acquisition occurrence", record.OccurrenceID, 256}, {"media acquisition source", record.SourceID, 256},
		{"media acquisition origin", record.OriginID, 256}, {"media acquisition stage", record.Stage, 64},
	} {
		if err := validateBoundedMediaText(field.name, field.value, field.maxBytes, false); err != nil {
			return err
		}
	}
	if !canonical.IsSHA256Hex(record.ResolverFingerprint) || !canonical.IsSHA256Hex(record.RequestSHA256) {
		return errors.New("invalid media acquisition digest")
	}
	if !validMediaAcquisitionOutcome(record.Outcome) || !validMediaFailureCode(record.FailureCode) {
		return errors.New("invalid media acquisition result")
	}
	if (record.State == mediaOperationQueued || record.State == mediaOperationRunning) && record.Outcome != "" {
		return errors.New("unfinished media acquisition has an outcome")
	}
	if err := validateMetadataTime("media acquisition available_at", record.AvailableAt); err != nil {
		return err
	}
	for name, value := range map[string]*string{"started_at": record.StartedAt, "finished_at": record.FinishedAt} {
		if value != nil {
			if err := validateMetadataTime("media acquisition "+name, *value); err != nil {
				return err
			}
		}
	}
	return nil
}

func validMediaOperationState(state string) bool {
	switch state {
	case mediaOperationQueued, mediaOperationRunning, mediaOperationSucceeded, mediaOperationFailed, "cancelled":
		return true
	default:
		return false
	}
}

func validMediaAcquisitionOutcome(outcome string) bool {
	switch outcome {
	case "", "metadata_only", "content_available", "access_required", "unsupported", "failure_transient", "failure_terminal":
		return true
	default:
		return false
	}
}

func validMediaFailureCode(code string) bool {
	switch code {
	case "", "origin_unregistered", "redirect_unregistered", "redirect_limit", "dns_denied", "scheme_downgrade",
		"userinfo_present", "credential_cross_origin", "tls_pin_mismatch", "byte_limit", "duration_limit",
		"time_limit", "partial_download", "mime_mismatch", "digest_mismatch", "password_required",
		"export_disabled", "provider_deleted", "provider_unavailable", "provider_rate_limited",
		"credential_missing", "consent_absent", "consent_revoked", "unqualified_codec", "timing_unavailable",
		"segment_limit", "claim_stale", "occurrence_conflict", "operation_conflict":
		return true
	default:
		return false
	}
}

func canonicalJSONText(raw, subject string) ([]byte, error) {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return nil, fmt.Errorf("%s must be bounded canonical JSON", subject)
	}
	if jsontext.Value(raw).Kind() != jsontext.KindBeginObject {
		return nil, fmt.Errorf("%s must be a JSON object", subject)
	}
	value, err := canonicalCatalogJSON(jsontext.Value(raw), subject)
	if err != nil || !bytes.Equal(value, []byte(raw)) {
		return nil, fmt.Errorf("%s must be bounded canonical JSON", subject)
	}
	return value, nil
}

func validateMediaMetadataState(ctx context.Context, query metadataQuerier) error {
	for _, table := range []string{
		"media_sources", "media_source_versions", "media_source_heads", "media_occurrences",
		"media_visibility_fences", "media_input_artifacts", "media_operations", "media_acquisitions", "media_protected_refs",
	} {
		var count int64
		if err := query.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			return fmt.Errorf("validating current media table %s: %w", table, err)
		}
	}
	checks := []struct{ message, query string }{
		{"media source version differs from core content", `SELECT EXISTS(SELECT 1 FROM media_source_versions m LEFT JOIN content_versions v ON v.version_id=m.content_version_id WHERE v.version_id IS NULL OR m.source_sha256<>v.blob_hash OR m.source_bytes<>v.size)`},
		{"media source head crosses source identity", `SELECT EXISTS(SELECT 1 FROM media_source_heads h LEFT JOIN media_source_versions v ON v.source_version_id=h.source_version_id WHERE h.source_version_id IS NOT NULL AND (v.source_version_id IS NULL OR v.source_id<>h.source_id OR v.revision<>h.revision))`},
		{"media occurrence crosses source identity", `SELECT EXISTS(SELECT 1 FROM media_occurrences o LEFT JOIN media_source_versions v ON v.source_version_id=o.source_version_id WHERE o.source_version_id IS NOT NULL AND (v.source_version_id IS NULL OR v.source_id<>o.source_id))`},
		{"media occurrence lacks a visibility fence", `SELECT EXISTS(SELECT 1 FROM media_occurrences o LEFT JOIN media_visibility_fences f ON f.caller_principal=o.caller_principal WHERE f.caller_principal IS NULL)`},
		{"media input artifact crosses occurrence or version authority", `SELECT EXISTS(SELECT 1 FROM media_input_artifacts i LEFT JOIN media_occurrences o ON o.occurrence_id=i.occurrence_id LEFT JOIN media_source_versions v ON v.source_version_id=i.source_version_id LEFT JOIN content_versions c ON c.version_id=i.content_version_id WHERE o.occurrence_id IS NULL OR o.source_id<>i.source_id OR c.version_id IS NULL OR c.blob_hash<>i.input_sha256 OR (i.source_version_id IS NOT NULL AND (v.source_version_id IS NULL OR v.source_id<>i.source_id OR o.source_version_id IS NULL OR o.source_version_id<>i.source_version_id)))`},
		{"media acquisition crosses operation or occurrence authority", `SELECT EXISTS(SELECT 1 FROM media_acquisitions a LEFT JOIN media_operations p ON p.operation_id=a.operation_id LEFT JOIN media_occurrences o ON o.occurrence_id=a.occurrence_id WHERE p.operation_id IS NULL OR o.occurrence_id IS NULL OR (p.source_id IS NOT NULL AND p.source_id<>a.source_id) OR o.source_id<>a.source_id)`},
		{"media protected reference crosses occurrence authority", `SELECT EXISTS(SELECT 1 FROM media_protected_refs p JOIN media_acquisitions a ON a.acquisition_id=p.acquisition_id WHERE p.occurrence_id<>a.occurrence_id)`},
	}
	for _, check := range checks {
		var mismatch bool
		if err := query.QueryRowContext(ctx, check.query).Scan(&mismatch); err != nil {
			return fmt.Errorf("validating media authority: %w", err)
		}
		if mismatch {
			return errors.New(check.message)
		}
	}
	return exportMediaMetadata(ctx, query, func(any) error { return nil })
}
