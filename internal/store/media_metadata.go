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
	metadataMediaSourceType        = "media_source"
	metadataMediaSourceVersionType = "media_source_version"
	metadataMediaOccurrenceType    = "media_occurrence"
	metadataMediaInputArtifactType = "media_input_artifact"
	metadataMediaOperationType     = "media_operation"
)

var mediaMetadataRequiredFields = map[string][]string{
	metadataMediaSourceType:        {metadataTypeField, "source_id", "kind", "provider", "origin_scope", "identity_sha256", "created_at"},
	metadataMediaSourceVersionType: {metadataTypeField, auditSourceVersionIDField, "source_id", "revision", "content_version_id", "capture_json", "created_at"},
	metadataMediaOccurrenceType:    {metadataTypeField, "occurrence_id", "source_id", auditSourceVersionIDField, "caller_principal", "caller_occurrence_ref", "caller_revision", "caller_filename", "caller_person_ref", "speaker_label", "message_json", "visible", "first_seen_at", "revoked_at"},
	metadataMediaInputArtifactType: {metadataTypeField, "input_id", "occurrence_id", "source_id", auditSourceVersionIDField, "content_version_id", "kind", "origin", "provider", "language", "input_sha256", "created_at"},
	metadataMediaOperationType:     {metadataTypeField, "operation_id", "principal", "verb", "request_sha256", "source_id", "receipt_json", "created_at", "updated_at"},
}

var mediaMetadataNullableFields = map[string]map[string]bool{
	metadataMediaOccurrenceType:    {auditSourceVersionIDField: true, "revoked_at": true},
	metadataMediaInputArtifactType: {auditSourceVersionIDField: true},
	metadataMediaOperationType:     {"source_id": true},
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
	CaptureJSON      string `json:"capture_json"`
	CreatedAt        string `json:"created_at"`
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
	SourceID      *string `json:"source_id"`
	ReceiptJSON   string  `json:"receipt_json"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

func exportMediaMetadata(ctx context.Context, query metadataQuerier, write metadataWrite) error {
	for _, exporter := range []func(context.Context, metadataQuerier, metadataWrite) error{
		exportMediaSources, exportMediaSourceVersions, exportMediaOccurrences, exportMediaInputArtifacts, exportMediaOperations,
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
		`SELECT source_version_id,source_id,revision,content_version_id,capture_json,created_at FROM media_source_versions ORDER BY source_version_id`,
		func(rows *sql.Rows) (any, error) {
			record := metadataMediaSourceVersion{Type: metadataMediaSourceVersionType}
			err := rows.Scan(&record.SourceVersionID, &record.SourceID, &record.Revision, &record.ContentVersionID, &record.CaptureJSON, &record.CreatedAt)
			if err == nil {
				err = validateMetadataMediaSourceVersion(record)
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
		`SELECT operation_id,principal,verb,request_sha256,source_id,receipt_json,created_at,updated_at FROM media_operations ORDER BY operation_id`,
		func(rows *sql.Rows) (any, error) {
			record := metadataMediaOperation{Type: metadataMediaOperationType}
			err := rows.Scan(&record.OperationID, &record.Principal, &record.Verb, &record.RequestSHA256, &record.SourceID, &record.ReceiptJSON, &record.CreatedAt, &record.UpdatedAt)
			if err == nil {
				err = validateMetadataMediaOperation(record)
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
		_, err := tx.ExecContext(ctx, `INSERT INTO media_source_versions VALUES(?,?,?,?,?,?)`, record.SourceVersionID, record.SourceID, record.Revision, record.ContentVersionID, record.CaptureJSON, record.CreatedAt)
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
		// Admission without a job cannot resume: restore grants no provider authority.
		if record.Verb == "submit_supplied_media" || record.Verb == "retry_media" {
			receipt, err := canonical.Decode[MediaPublicationReceipt]([]byte(record.ReceiptJSON))
			if err != nil {
				return err
			}
			if receipt.ProcessingProfile != "" && receipt.JobID == "" && receipt.OperationState == mediaOperationQueued {
				receipt.OperationState, receipt.CoverageState = MediaOperationFailed, mediaCoverageUnavailable
				encoded, err := canonical.Marshal(receipt)
				if err != nil {
					return err
				}
				record.ReceiptJSON = string(encoded)
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO media_operations VALUES(?,?,?,?,?,?,?,?)`, record.OperationID, record.Principal, record.Verb, record.RequestSHA256, record.SourceID, record.ReceiptJSON, record.CreatedAt, record.UpdatedAt)
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
	if record.Type != metadataMediaSourceVersionType || record.Revision < 1 {
		return errors.New("invalid media source version record")
	}
	for _, value := range []string{record.SourceVersionID, record.SourceID, record.ContentVersionID} {
		if err := validateBoundedMediaText("media source version identity", value, 256, false); err != nil {
			return err
		}
	}
	if err := canonicalJSONText(record.CaptureJSON, "media capture claim"); err != nil {
		return errors.New("invalid media capture claim")
	}
	return validateMetadataTime("media source version created_at", record.CreatedAt)
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
	if record.Type != metadataMediaOperationType {
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

func canonicalJSONText(raw, subject string) error {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return fmt.Errorf("%s must be bounded canonical JSON", subject)
	}
	if jsontext.Value(raw).Kind() != jsontext.KindBeginObject {
		return fmt.Errorf("%s must be a JSON object", subject)
	}
	value, err := canonicalCatalogJSON(jsontext.Value(raw), subject)
	if err != nil || !bytes.Equal(value, []byte(raw)) {
		return fmt.Errorf("%s must be bounded canonical JSON", subject)
	}
	return nil
}

func validateMediaMetadataState(ctx context.Context, query metadataQuerier) error {
	for _, table := range []string{
		"media_sources", "media_source_versions", "media_occurrences", "media_input_artifacts", "media_operations",
	} {
		var count int64
		if err := query.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			return fmt.Errorf("validating current media table %s: %w", table, err)
		}
	}
	checks := []struct{ message, query string }{
		{"media source version lacks core content", `SELECT EXISTS(SELECT 1 FROM media_source_versions m LEFT JOIN content_versions v ON v.version_id=m.content_version_id WHERE v.version_id IS NULL)`},
		{"media occurrence crosses source identity", `SELECT EXISTS(SELECT 1 FROM media_occurrences o LEFT JOIN media_source_versions v ON v.source_version_id=o.source_version_id WHERE o.source_version_id IS NOT NULL AND (v.source_version_id IS NULL OR v.source_id<>o.source_id))`},
		{"media input artifact crosses occurrence or version authority", `SELECT EXISTS(SELECT 1 FROM media_input_artifacts i LEFT JOIN media_occurrences o ON o.occurrence_id=i.occurrence_id LEFT JOIN media_source_versions v ON v.source_version_id=i.source_version_id LEFT JOIN content_versions c ON c.version_id=i.content_version_id WHERE o.occurrence_id IS NULL OR o.source_id<>i.source_id OR c.version_id IS NULL OR c.blob_hash<>i.input_sha256 OR (i.source_version_id IS NOT NULL AND (v.source_version_id IS NULL OR v.source_id<>i.source_id OR o.source_version_id IS NULL OR o.source_version_id<>i.source_version_id)))`},
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
