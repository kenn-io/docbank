package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"reflect"

	"go.kenn.io/docbank/document"
)

const (
	metadataEmbeddingVectorSpaceType    = "embedding_vector_space"
	metadataEmbeddingGenerationType     = "embedding_input_generation"
	metadataEmbeddingInputType          = "embedding_generation_input"
	metadataEmbeddingVectorSetType      = "embedding_vector_set"
	metadataEmbeddingVectorRowType      = "embedding_vector_row"
	metadataEmbeddingSetType            = "embedding_set"
	metadataEmbeddingHeadType           = "embedding_head"
	metadataEmbeddingFailureType        = "embedding_failure"
	metadataEmbeddingVectorSpaceIDField = "vector_space_id"
	metadataEmbeddingProfileField       = "profile_fingerprint"
)

type metadataEmbeddingVectorSpace struct {
	Type                  string `json:"type"`
	ID                    string `json:"vector_space_id"`
	ContractVersion       string `json:"contract_version"`
	DescriptorJSON        []byte `json:"descriptor_json"`
	ProviderDescriptor    string `json:"provider_descriptor"`
	ProviderRevision      string `json:"provider_revision"`
	DescriptorFingerprint string `json:"descriptor_fingerprint"`
	CompatibilityID       string `json:"compatibility_id"`
	Dimensions            int    `json:"dimensions"`
	Metric                string `json:"metric"`
	Normalization         string `json:"normalization"`
	ScalarEncoding        string `json:"scalar_encoding"`
	DocumentFormatter     string `json:"document_formatter"`
	QueryFormatter        string `json:"query_formatter"`
	ModelInputFingerprint string `json:"model_input_fingerprint"`
}

type metadataEmbeddingGeneration struct {
	Type                         string  `json:"type"`
	ID                           string  `json:"generation_id"`
	GenerationBlobHash           string  `json:"generation_blob_hash"`
	GenerationEncodedSize        int64   `json:"generation_encoded_size"`
	GenerationChecksum           string  `json:"generation_checksum"`
	SourceVersionID              string  `json:"source_version_id"`
	ProfileFingerprint           string  `json:"profile_fingerprint"`
	EvidenceFingerprint          string  `json:"evidence_fingerprint"`
	TokenizerFingerprint         string  `json:"tokenizer_fingerprint"`
	ChunkPolicyFingerprint       string  `json:"chunk_policy_fingerprint"`
	FormatterFingerprint         string  `json:"formatter_fingerprint"`
	AttachmentContextFingerprint string  `json:"attachment_context_fingerprint"`
	AttachmentID                 *string `json:"attachment_id"`
	InputCount                   int     `json:"input_count"`
	CreatedAt                    string  `json:"created_at"`
}

type metadataEmbeddingInput struct {
	Type             string `json:"type"`
	GenerationID     string `json:"generation_id"`
	InputID          string `json:"input_id"`
	Order            int    `json:"order"`
	RenderedChecksum string `json:"rendered_checksum"`
}

type metadataEmbeddingVectorSet struct {
	Type             string `json:"type"`
	ID               string `json:"vector_set_id"`
	ContractVersion  string `json:"contract_version"`
	VectorSpaceID    string `json:"vector_space_id"`
	PayloadBlobHash  string `json:"payload_blob_hash"`
	PayloadSize      int64  `json:"payload_size"`
	PayloadChecksum  string `json:"payload_checksum"`
	ManifestChecksum string `json:"manifest_checksum"`
	RowCount         int    `json:"row_count"`
	Dimensions       int    `json:"dimensions"`
}

type metadataEmbeddingVectorRow struct {
	Type        string `json:"type"`
	VectorSetID string `json:"vector_set_id"`
	RowID       string `json:"row_id"`
	Order       int    `json:"order"`
	InputID     string `json:"input_id"`
	Dimensions  int    `json:"dimensions"`
	Checksum    string `json:"checksum"`
}

type metadataEmbeddingSet struct {
	Type               string             `json:"type"`
	ID                 string             `json:"embedding_set_id"`
	VaultID            string             `json:"vault_id"`
	BindingID          string             `json:"binding_id"`
	InputKind          EmbeddingInputKind `json:"input_kind"`
	ContentVersionID   string             `json:"content_version_id"`
	ProfileFingerprint string             `json:"profile_fingerprint"`
	InputFingerprint   string             `json:"embedding_input_fingerprint"`
	VectorSpaceID      string             `json:"vector_space_id"`
	GenerationID       string             `json:"generation_id"`
	VectorSetID        string             `json:"vector_set_id"`
	CreatedAt          string             `json:"created_at"`
}

type metadataEmbeddingHead struct {
	Type               string             `json:"type"`
	ContentVersionID   string             `json:"content_version_id"`
	BindingID          string             `json:"binding_id"`
	InputKind          EmbeddingInputKind `json:"input_kind"`
	SetID              string             `json:"embedding_set_id"`
	VectorSpaceID      string             `json:"vector_space_id"`
	ProfileFingerprint string             `json:"profile_fingerprint"`
	PublishedAt        string             `json:"published_at"`
}

type metadataEmbeddingFailure struct {
	Type               string               `json:"type"`
	ContentVersionID   string               `json:"content_version_id"`
	ProfileFingerprint string               `json:"profile_fingerprint"`
	BindingID          string               `json:"binding_id"`
	InputKind          EmbeddingInputKind   `json:"input_kind"`
	FailureCode        EmbeddingFailureCode `json:"failure_code"`
	FailedAt           string               `json:"failed_at"`
}

var embeddingMetadataRequiredFields = map[string][]string{
	metadataEmbeddingVectorSpaceType: {metadataTypeField, metadataEmbeddingVectorSpaceIDField, "contract_version", "descriptor_json", "provider_descriptor", "provider_revision", "descriptor_fingerprint", "compatibility_id", "dimensions", "metric", "normalization", "scalar_encoding", "document_formatter", "query_formatter", "model_input_fingerprint"},
	metadataEmbeddingGenerationType:  {metadataTypeField, metadataGenerationIDField, "generation_blob_hash", "generation_encoded_size", "generation_checksum", auditSourceVersionIDField, metadataEmbeddingProfileField, "evidence_fingerprint", "tokenizer_fingerprint", "chunk_policy_fingerprint", "formatter_fingerprint", "attachment_context_fingerprint", "attachment_id", "input_count", metadataCreatedAtField},
	metadataEmbeddingInputType:       {metadataTypeField, metadataGenerationIDField, "input_id", "order", "rendered_checksum"},
	metadataEmbeddingVectorSetType:   {metadataTypeField, "vector_set_id", "contract_version", metadataEmbeddingVectorSpaceIDField, "payload_blob_hash", "payload_size", "payload_checksum", "manifest_checksum", "row_count", "dimensions"},
	metadataEmbeddingVectorRowType:   {metadataTypeField, "vector_set_id", "row_id", "order", "input_id", "dimensions", "checksum"},
	metadataEmbeddingSetType:         {metadataTypeField, "embedding_set_id", auditVaultIDField, "binding_id", "input_kind", metadataContentVersionIDField, metadataEmbeddingProfileField, "embedding_input_fingerprint", metadataEmbeddingVectorSpaceIDField, metadataGenerationIDField, "vector_set_id", metadataCreatedAtField},
	metadataEmbeddingHeadType:        {metadataTypeField, metadataContentVersionIDField, "binding_id", "input_kind", "embedding_set_id", metadataEmbeddingVectorSpaceIDField, metadataEmbeddingProfileField, "published_at"},
	metadataEmbeddingFailureType:     {metadataTypeField, metadataContentVersionIDField, metadataEmbeddingProfileField, "binding_id", "input_kind", "failure_code", "failed_at"},
}

func exportEmbeddingMetadata(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	exports := []func(context.Context, metadataQuerier, metadataWrite) error{
		exportEmbeddingVectorSpaces, exportEmbeddingGenerations, exportEmbeddingInputs,
		exportEmbeddingVectorSets, exportEmbeddingVectorRows, exportEmbeddingSets,
		exportEmbeddingHeads, exportEmbeddingFailures,
	}
	for _, export := range exports {
		if err := export(ctx, tx, write); err != nil {
			return err
		}
	}
	return nil
}

func exportEmbeddingVectorSpaces(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT vector_space_id,contract_version,descriptor_json,provider_descriptor,
		provider_revision,descriptor_fingerprint,compatibility_id,dimensions,metric,
		normalization,scalar_encoding,document_formatter,query_formatter,model_input_fingerprint
		FROM embedding_vector_spaces ORDER BY vector_space_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataEmbeddingVectorSpace{Type: metadataEmbeddingVectorSpaceType}
		if err := rows.Scan(&value.ID, &value.ContractVersion, &value.DescriptorJSON, &value.ProviderDescriptor,
			&value.ProviderRevision, &value.DescriptorFingerprint, &value.CompatibilityID,
			&value.Dimensions, &value.Metric, &value.Normalization, &value.ScalarEncoding,
			&value.DocumentFormatter, &value.QueryFormatter, &value.ModelInputFingerprint); err != nil {
			return err
		}
		if err := write(value); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportEmbeddingGenerations(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT generation_id,generation_blob_hash,generation_encoded_size,generation_checksum,source_version_id,
		profile_fingerprint,evidence_fingerprint,tokenizer_fingerprint,chunk_policy_fingerprint,
		formatter_fingerprint,attachment_context_fingerprint,attachment_id,
		input_count,created_at FROM embedding_input_generations ORDER BY generation_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataEmbeddingGeneration{Type: metadataEmbeddingGenerationType}
		var generationBlob sql.NullString
		var attachment sql.NullString
		if err := rows.Scan(&value.ID, &generationBlob, &value.GenerationEncodedSize, &value.GenerationChecksum, &value.SourceVersionID,
			&value.ProfileFingerprint, &value.EvidenceFingerprint, &value.TokenizerFingerprint,
			&value.ChunkPolicyFingerprint, &value.FormatterFingerprint,
			&value.AttachmentContextFingerprint, &attachment,
			&value.InputCount, &value.CreatedAt); err != nil {
			return err
		}
		if attachment.Valid {
			value.AttachmentID = &attachment.String
		}
		if generationBlob.Valid {
			value.GenerationBlobHash = generationBlob.String
		}
		if err := write(value); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportEmbeddingInputs(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT generation_id,input_id,input_order,rendered_checksum
		FROM embedding_generation_inputs ORDER BY generation_id,input_order`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataEmbeddingInput{Type: metadataEmbeddingInputType}
		if err := rows.Scan(&value.GenerationID, &value.InputID, &value.Order, &value.RenderedChecksum); err != nil {
			return err
		}
		if err := write(value); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportEmbeddingVectorSets(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT vector_set_id,contract_version,vector_space_id,payload_blob_hash,
		payload_size,payload_checksum,manifest_checksum,row_count,dimensions FROM embedding_vector_sets ORDER BY vector_set_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataEmbeddingVectorSet{Type: metadataEmbeddingVectorSetType}
		if err := rows.Scan(&value.ID, &value.ContractVersion, &value.VectorSpaceID, &value.PayloadBlobHash, &value.PayloadSize,
			&value.PayloadChecksum, &value.ManifestChecksum, &value.RowCount, &value.Dimensions); err != nil {
			return err
		}
		if err := write(value); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportEmbeddingVectorRows(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT vector_set_id,row_id,row_order,input_id,dimensions,checksum
		FROM embedding_vector_rows ORDER BY vector_set_id,row_order`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataEmbeddingVectorRow{Type: metadataEmbeddingVectorRowType}
		if err := rows.Scan(&value.VectorSetID, &value.RowID, &value.Order, &value.InputID,
			&value.Dimensions, &value.Checksum); err != nil {
			return err
		}
		if err := write(value); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportEmbeddingSets(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT embedding_set_id,vault_uid,binding_id,input_kind,
		content_version_id,profile_fingerprint,embedding_input_fingerprint,vector_space_id,input_generation_id,
		vector_set_id,created_at FROM embedding_sets ORDER BY embedding_set_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataEmbeddingSet{Type: metadataEmbeddingSetType}
		if err := rows.Scan(&value.ID, &value.VaultID, &value.BindingID, &value.InputKind,
			&value.ContentVersionID, &value.ProfileFingerprint, &value.InputFingerprint, &value.VectorSpaceID,
			&value.GenerationID, &value.VectorSetID, &value.CreatedAt); err != nil {
			return err
		}
		if err := write(value); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportEmbeddingHeads(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT content_version_id,binding_id,input_kind,
		embedding_set_id,vector_space_id,profile_fingerprint,published_at
		FROM embedding_heads ORDER BY content_version_id,profile_fingerprint,binding_id,input_kind`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataEmbeddingHead{Type: metadataEmbeddingHeadType}
		if err := rows.Scan(&value.ContentVersionID, &value.BindingID, &value.InputKind,
			&value.SetID, &value.VectorSpaceID, &value.ProfileFingerprint, &value.PublishedAt); err != nil {
			return err
		}
		if err := write(value); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportEmbeddingFailures(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT content_version_id,profile_fingerprint,binding_id,
		input_kind,failure_code,failed_at FROM embedding_failures
		ORDER BY content_version_id,profile_fingerprint,binding_id,input_kind`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataEmbeddingFailure{Type: metadataEmbeddingFailureType}
		if err := rows.Scan(&value.ContentVersionID, &value.ProfileFingerprint, &value.BindingID,
			&value.InputKind, &value.FailureCode, &value.FailedAt); err != nil {
			return err
		}
		if err := write(value); err != nil {
			return err
		}
	}
	return rows.Err()
}

func importEmbeddingMetadataRecord(ctx context.Context, tx *sql.Tx, kind string, raw jsontext.Value) error {
	switch kind {
	case metadataEmbeddingVectorSpaceType:
		var value metadataEmbeddingVectorSpace
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if _, err := decodeCanonicalEmbeddingDescriptor(value.DescriptorJSON); err != nil {
			return err
		}
		return execEmbeddingImport(ctx, tx, `INSERT INTO embedding_vector_spaces(
			vector_space_id,contract_version,descriptor_json,provider_descriptor,provider_revision,
			descriptor_fingerprint,compatibility_id,dimensions,metric,normalization,scalar_encoding,
			document_formatter,query_formatter,model_input_fingerprint) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, value.ID, value.ContractVersion, value.DescriptorJSON, value.ProviderDescriptor, value.ProviderRevision, value.DescriptorFingerprint, value.CompatibilityID, value.Dimensions, value.Metric, value.Normalization, value.ScalarEncoding, value.DocumentFormatter, value.QueryFormatter, value.ModelInputFingerprint)
	case metadataEmbeddingGenerationType:
		var value metadataEmbeddingGeneration
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if value.InputCount < 1 || value.InputCount > maxEmbeddingCatalogRows {
			return errors.New("embedding input count exceeds bounds")
		}
		if (value.GenerationBlobHash == "") != (value.GenerationEncodedSize == 0) ||
			value.GenerationEncodedSize < 0 || value.GenerationEncodedSize > 64<<20 {
			return errors.New("embedding generation artifact reference exceeds bounds")
		}
		if err := validateMetadataTime("input-generation created_at", value.CreatedAt); err != nil {
			return err
		}
		return execEmbeddingImport(ctx, tx, `INSERT INTO embedding_input_generations(
			generation_id,generation_blob_hash,generation_encoded_size,generation_checksum,source_version_id,profile_fingerprint,evidence_fingerprint,
			tokenizer_fingerprint,chunk_policy_fingerprint,formatter_fingerprint,
			attachment_context_fingerprint,attachment_id,input_count,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, value.ID, nullableCatalogString(value.GenerationBlobHash), value.GenerationEncodedSize, value.GenerationChecksum, value.SourceVersionID, value.ProfileFingerprint, value.EvidenceFingerprint, value.TokenizerFingerprint, value.ChunkPolicyFingerprint, value.FormatterFingerprint, value.AttachmentContextFingerprint, value.AttachmentID, value.InputCount, value.CreatedAt)
	case metadataEmbeddingInputType:
		var value metadataEmbeddingInput
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if value.Order < 0 || value.Order >= maxEmbeddingCatalogRows {
			return errors.New("embedding input order exceeds bounds")
		}
		return execEmbeddingImport(ctx, tx, `INSERT INTO embedding_generation_inputs VALUES(?,?,?,?)`, value.GenerationID, value.InputID, value.Order, value.RenderedChecksum)
	case metadataEmbeddingVectorSetType:
		var value metadataEmbeddingVectorSet
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if value.RowCount < 1 || value.RowCount > maxEmbeddingCatalogRows {
			return errors.New("embedding vector row count exceeds bounds")
		}
		if value.PayloadSize < 1 || value.PayloadSize > 64<<20 {
			return errors.New("embedding vector payload size exceeds bounds")
		}
		return execEmbeddingImport(ctx, tx, `INSERT INTO embedding_vector_sets(
			vector_set_id,contract_version,vector_space_id,payload_blob_hash,payload_size,payload_checksum,
			manifest_checksum,row_count,dimensions) VALUES(?,?,?,?,?,?,?,?,?)`, value.ID, value.ContractVersion, value.VectorSpaceID, value.PayloadBlobHash, value.PayloadSize, value.PayloadChecksum, value.ManifestChecksum, value.RowCount, value.Dimensions)
	case metadataEmbeddingVectorRowType:
		var value metadataEmbeddingVectorRow
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if value.Order < 0 || value.Order >= maxEmbeddingCatalogRows {
			return errors.New("embedding vector row order exceeds bounds")
		}
		return execEmbeddingImport(ctx, tx, `INSERT INTO embedding_vector_rows VALUES(?,?,?,?,?,?)`, value.VectorSetID, value.RowID, value.Order, value.InputID, value.Dimensions, value.Checksum)
	case metadataEmbeddingSetType:
		var value metadataEmbeddingSet
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if err := validateMetadataTime("embedding set created_at", value.CreatedAt); err != nil {
			return err
		}
		return execEmbeddingImport(ctx, tx, `INSERT INTO embedding_sets(
			embedding_set_id,vault_uid,binding_id,input_kind,content_version_id,profile_fingerprint,
			embedding_input_fingerprint,vector_space_id,input_generation_id,vector_set_id,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, value.ID, value.VaultID, value.BindingID, value.InputKind, value.ContentVersionID, value.ProfileFingerprint, value.InputFingerprint, value.VectorSpaceID, value.GenerationID, value.VectorSetID, value.CreatedAt)
	case metadataEmbeddingHeadType:
		var value metadataEmbeddingHead
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if err := validateMetadataTime("embedding head published_at", value.PublishedAt); err != nil {
			return err
		}
		return execEmbeddingImport(ctx, tx, `INSERT INTO embedding_heads VALUES(?,?,?,?,?,?,?)`, value.ContentVersionID, value.BindingID, value.InputKind, value.SetID, value.VectorSpaceID, value.ProfileFingerprint, value.PublishedAt)
	case metadataEmbeddingFailureType:
		var value metadataEmbeddingFailure
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if err := validateMetadataTime("embedding failure failed_at", value.FailedAt); err != nil {
			return err
		}
		return execEmbeddingImport(ctx, tx, `INSERT INTO embedding_failures VALUES(?,?,?,?,?,?)`, value.ContentVersionID, value.ProfileFingerprint, value.BindingID, value.InputKind, value.FailureCode, value.FailedAt)
	default:
		return fmt.Errorf("unknown embedding metadata type %q", kind)
	}
}

func execEmbeddingImport(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

func isEmbeddingMetadataType(kind string) bool {
	_, ok := embeddingMetadataRequiredFields[kind]
	return ok
}

func validateEmbeddingMetadataState(ctx context.Context, tx metadataQuerier) (retErr error) {
	headRows, err := tx.QueryContext(ctx, `SELECT content_version_id,binding_id,input_kind,
		embedding_set_id,vector_space_id,profile_fingerprint,published_at
		FROM embedding_heads ORDER BY content_version_id,profile_fingerprint,binding_id,input_kind`)
	if err != nil {
		return fmt.Errorf("listing embedding heads for validation: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, headRows.Close()) }()
	for headRows.Next() {
		var record EmbeddingHeadRecord
		if err := headRows.Scan(&record.Key.ContentVersionID, &record.Key.BindingID,
			&record.Key.InputKind, &record.SetID, &record.VectorSpaceID,
			&record.ProcessingProfileFingerprint, &record.PublishedAt); err != nil {
			return fmt.Errorf("reading embedding head for validation: %w", err)
		}
		if err := validateEmbeddingHeadRecord(record); err != nil {
			return fmt.Errorf("validating embedding head: %w", err)
		}
	}
	if err := headRows.Err(); err != nil {
		return fmt.Errorf("iterating embedding heads for validation: %w", err)
	}
	if err := headRows.Close(); err != nil {
		return fmt.Errorf("closing embedding heads after validation: %w", err)
	}

	failureRows, err := tx.QueryContext(ctx, `SELECT content_version_id,profile_fingerprint,
		binding_id,input_kind,failure_code,failed_at
		FROM embedding_failures ORDER BY content_version_id,profile_fingerprint,binding_id,input_kind`)
	if err != nil {
		return fmt.Errorf("listing embedding failures for validation: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, failureRows.Close()) }()
	var failures []EmbeddingFailureRecord
	for failureRows.Next() {
		var record EmbeddingFailureRecord
		if err := failureRows.Scan(&record.ContentVersionID,
			&record.ProcessingProfileFingerprint, &record.BindingID, &record.InputKind,
			&record.FailureCode, &record.FailedAt); err != nil {
			return fmt.Errorf("reading embedding failure for validation: %w", err)
		}
		if err := validateEmbeddingFailureRecord(record); err != nil {
			return fmt.Errorf("validating embedding failure: %w", err)
		}
		failures = append(failures, record)
	}
	if err := failureRows.Err(); err != nil {
		return fmt.Errorf("iterating embedding failures for validation: %w", err)
	}
	if err := failureRows.Close(); err != nil {
		return fmt.Errorf("closing embedding failures after validation: %w", err)
	}
	for _, record := range failures {
		if err := validateEmbeddingFailureBinding(ctx, tx, record); err != nil {
			return fmt.Errorf("validating embedding failure profile binding: %w", err)
		}
	}

	var invalid int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM embedding_input_generations g WHERE g.input_count !=
		 (SELECT COUNT(*) FROM embedding_generation_inputs i WHERE i.generation_id=g.generation_id)) +
		(SELECT COUNT(*) FROM embedding_vector_sets s WHERE s.row_count !=
		 (SELECT COUNT(*) FROM embedding_vector_rows r WHERE r.vector_set_id=s.vector_set_id)) +
		(SELECT COUNT(*) FROM embedding_heads h JOIN embedding_sets s ON s.embedding_set_id=h.embedding_set_id
		 WHERE h.content_version_id!=s.content_version_id OR h.binding_id!=s.binding_id OR
		 h.input_kind!=s.input_kind OR h.vector_space_id!=s.vector_space_id OR
		 h.profile_fingerprint!=s.profile_fingerprint)`).Scan(&invalid); err != nil {
		return fmt.Errorf("validating embedding catalog counts: %w", err)
	}
	if invalid != 0 {
		return errors.New("embedding catalog count or head authority is corrupt")
	}
	spaceIDs, err := loadProcessingMetadataIDs(ctx, tx, "embedding vector space",
		`SELECT vector_space_id FROM embedding_vector_spaces ORDER BY vector_space_id`)
	if err != nil {
		return err
	}
	for _, id := range spaceIDs {
		space, err := loadVectorSpaceTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := validateEmbeddingVectorSpace(space); err != nil {
			return fmt.Errorf("validating embedding vector space %s: %w", id, err)
		}
	}
	generationIDs, err := loadProcessingMetadataIDs(ctx, tx, "embedding input generation",
		`SELECT generation_id FROM embedding_input_generations ORDER BY generation_id`)
	if err != nil {
		return err
	}
	for _, id := range generationIDs {
		generation, err := loadInputGenerationTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := validateEmbeddingInputGeneration(generation); err != nil {
			return fmt.Errorf("validating embedding input generation %s: %w", id, err)
		}
	}
	vectorSetIDs, err := loadProcessingMetadataIDs(ctx, tx, "embedding vector set",
		`SELECT vector_set_id FROM embedding_vector_sets ORDER BY vector_set_id`)
	if err != nil {
		return err
	}
	for _, id := range vectorSetIDs {
		vectorSet, err := loadVectorSetTx(ctx, tx, id)
		if err != nil {
			return err
		}
		space, err := loadVectorSpaceTx(ctx, tx, vectorSet.VectorSpaceID)
		if err != nil {
			return err
		}
		generation := EmbeddingInputGenerationRecord{Inputs: make([]EmbeddingInputReference, len(vectorSet.rows))}
		for index, row := range vectorSet.rows {
			generation.Inputs[index] = EmbeddingInputReference{ID: row.InputID, RenderedChecksum: row.Checksum}
		}
		if err := validateEmbeddingVectorSet(vectorSet, space, generation); err != nil {
			return fmt.Errorf("validating embedding vector set %s: %w", id, err)
		}
	}
	setIDs, err := loadProcessingMetadataIDs(ctx, tx, "embedding set", `SELECT embedding_set_id FROM embedding_sets ORDER BY embedding_set_id`)
	if err != nil {
		return err
	}
	for _, id := range setIDs {
		set, err := loadEmbeddingSetTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := validateEmbeddingSetRecord(set); err != nil {
			return fmt.Errorf("validating embedding set %s: %w", id, err)
		}
		if err := validateRestoredEmbeddingAuthority(ctx, tx, set); err != nil {
			return fmt.Errorf("validating restored embedding set %s: %w", id, err)
		}
	}
	return nil
}

func validateRestoredEmbeddingAuthority(ctx context.Context, tx metadataQuerier, set EmbeddingSetRecord) error {
	descriptor, err := document.NewEmbeddingDescriptor(set.VectorSpace.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, set.VectorSpace.Descriptor) {
		return errors.New("stored E1 descriptor is not canonical")
	}
	binding, fingerprints, err := embeddingProfileBindingAuthority(ctx, tx, set.ProcessingProfileFingerprint, set.BindingID)
	if err != nil {
		return err
	}
	if err := validateEmbeddingBindingAuthority(set, binding, fingerprints); err != nil {
		return err
	}
	if set.InputKind == document.EmbeddingInputOriginalFile {
		var sourceHash string
		if err := tx.QueryRowContext(ctx, `SELECT blob_hash FROM content_versions WHERE version_id=?`, set.ContentVersionID).Scan(&sourceHash); err != nil {
			return err
		}
		if set.InputGeneration.GenerationBlobHash != "" || set.InputGeneration.GenerationEncodedSize != 0 ||
			set.InputGeneration.AttachmentID != "" || len(set.InputGeneration.Inputs) != 1 ||
			set.InputGeneration.GenerationChecksum != sourceHash ||
			set.InputGeneration.Inputs[0] != (EmbeddingInputReference{ID: set.ContentVersionID, RenderedChecksum: sourceHash}) {
			return errors.New("stored original-file generation does not match source bytes")
		}
		return nil
	}
	if set.InputGeneration.GenerationBlobHash == "" || set.InputGeneration.GenerationEncodedSize < 2 {
		return errors.New("stored E2 generation artifact reference is missing")
	}
	expectedGenerationID := hashCatalogText("embedding-generation-attachment/v1\x00" + set.InputGeneration.GenerationChecksum + "\x00" + set.InputGeneration.AttachmentID)
	if set.InputGeneration.ID != expectedGenerationID {
		return errors.New("stored E2 generation identity does not match attachment-fenced checksum")
	}
	var evidenceChecksum string
	if err := tx.QueryRowContext(ctx, `SELECT b.evidence_checksum FROM rendition_attachments a
		JOIN rendition_builds b ON b.build_id=a.build_id WHERE a.attachment_id=?
		AND a.content_version_id=? AND a.profile_fingerprint=?`, set.InputGeneration.AttachmentID,
		set.ContentVersionID, set.ProcessingProfileFingerprint).Scan(&evidenceChecksum); err != nil {
		return err
	}
	if evidenceChecksum != set.InputGeneration.EvidenceFingerprint {
		return errors.New("stored E2 generation evidence does not match attachment")
	}
	return nil
}
