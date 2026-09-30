package store

import (
	"context"
	"database/sql"
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
	ID                    string `json:"vector_space_id" db:"vector_space_id"`
	ContractVersion       string `json:"contract_version" db:"contract_version"`
	DescriptorJSON        []byte `json:"descriptor_json" db:"descriptor_json"`
	ProviderDescriptor    string `json:"provider_descriptor" db:"provider_descriptor"`
	ProviderRevision      string `json:"provider_revision" db:"provider_revision"`
	DescriptorFingerprint string `json:"descriptor_fingerprint" db:"descriptor_fingerprint"`
	CompatibilityID       string `json:"compatibility_id" db:"compatibility_id"`
	Dimensions            int    `json:"dimensions" db:"dimensions"`
	Metric                string `json:"metric" db:"metric"`
	Normalization         string `json:"normalization" db:"normalization"`
	ScalarEncoding        string `json:"scalar_encoding" db:"scalar_encoding"`
	DocumentFormatter     string `json:"document_formatter" db:"document_formatter"`
	QueryFormatter        string `json:"query_formatter" db:"query_formatter"`
	ModelInputFingerprint string `json:"model_input_fingerprint" db:"model_input_fingerprint"`
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
	GenerationID     string `json:"generation_id" db:"generation_id"`
	InputID          string `json:"input_id" db:"input_id"`
	Order            int    `json:"order" db:"input_order"`
	RenderedChecksum string `json:"rendered_checksum" db:"rendered_checksum"`
}

type metadataEmbeddingVectorSet struct {
	Type             string `json:"type"`
	ID               string `json:"vector_set_id" db:"vector_set_id"`
	ContractVersion  string `json:"contract_version" db:"contract_version"`
	VectorSpaceID    string `json:"vector_space_id" db:"vector_space_id"`
	PayloadBlobHash  string `json:"payload_blob_hash" db:"payload_blob_hash"`
	PayloadSize      int64  `json:"payload_size" db:"payload_size"`
	PayloadChecksum  string `json:"payload_checksum" db:"payload_checksum"`
	ManifestChecksum string `json:"manifest_checksum" db:"manifest_checksum"`
	RowCount         int    `json:"row_count" db:"row_count"`
	Dimensions       int    `json:"dimensions" db:"dimensions"`
}

type metadataEmbeddingVectorRow struct {
	Type        string `json:"type"`
	VectorSetID string `json:"vector_set_id" db:"vector_set_id"`
	RowID       string `json:"row_id" db:"row_id"`
	Order       int    `json:"order" db:"row_order"`
	InputID     string `json:"input_id" db:"input_id"`
	Dimensions  int    `json:"dimensions" db:"dimensions"`
	Checksum    string `json:"checksum" db:"checksum"`
}

type metadataEmbeddingSet struct {
	Type               string             `json:"type"`
	ID                 string             `json:"embedding_set_id" db:"embedding_set_id"`
	VaultID            string             `json:"vault_id" db:"vault_uid"`
	BindingID          string             `json:"binding_id" db:"binding_id"`
	InputKind          EmbeddingInputKind `json:"input_kind" db:"input_kind"`
	ContentVersionID   string             `json:"content_version_id" db:"content_version_id"`
	ProfileFingerprint string             `json:"profile_fingerprint" db:"profile_fingerprint"`
	InputFingerprint   string             `json:"embedding_input_fingerprint" db:"embedding_input_fingerprint"`
	VectorSpaceID      string             `json:"vector_space_id" db:"vector_space_id"`
	GenerationID       string             `json:"generation_id" db:"input_generation_id"`
	VectorSetID        string             `json:"vector_set_id" db:"vector_set_id"`
	CreatedAt          string             `json:"created_at" db:"created_at"`
}

type metadataEmbeddingHead struct {
	Type               string             `json:"type"`
	ContentVersionID   string             `json:"content_version_id" db:"content_version_id"`
	BindingID          string             `json:"binding_id" db:"binding_id"`
	InputKind          EmbeddingInputKind `json:"input_kind" db:"input_kind"`
	SetID              string             `json:"embedding_set_id" db:"embedding_set_id"`
	VectorSpaceID      string             `json:"vector_space_id" db:"vector_space_id"`
	ProfileFingerprint string             `json:"profile_fingerprint" db:"profile_fingerprint"`
	PublishedAt        string             `json:"published_at" db:"published_at"`
	FencingToken       int64              `json:"fencing_token" db:"fencing_token"`
}

type metadataEmbeddingFailure struct {
	Type               string               `json:"type"`
	ContentVersionID   string               `json:"content_version_id" db:"content_version_id"`
	ProfileFingerprint string               `json:"profile_fingerprint" db:"profile_fingerprint"`
	BindingID          string               `json:"binding_id" db:"binding_id"`
	InputKind          EmbeddingInputKind   `json:"input_kind" db:"input_kind"`
	FailureCode        EmbeddingFailureCode `json:"failure_code" db:"failure_code"`
	FailedAt           string               `json:"failed_at" db:"failed_at"`
	FencingToken       int64                `json:"fencing_token" db:"fencing_token"`
	AttachmentID       string               `json:"attachment_id" db:"attachment_id"`
}

// embeddingMetadataTables exports the embedding records in dependency order.
var embeddingMetadataTables = []metadataRecordCodec{
	newMetadataTable(metadataTable[metadataEmbeddingVectorSpace]{
		record: metadataEmbeddingVectorSpace{Type: metadataEmbeddingVectorSpaceType}, table: "embedding_vector_spaces",
		suffix: "ORDER BY vector_space_id",
		validate: func(value metadataEmbeddingVectorSpace) error {
			_, err := decodeCanonicalEmbeddingDescriptor(value.DescriptorJSON)
			return err
		}}),
	newMetadataTable(metadataTable[metadataEmbeddingGeneration]{
		record: metadataEmbeddingGeneration{Type: metadataEmbeddingGenerationType}, table: "embedding_input_generations",
		validate: validateMetadataEmbeddingGeneration, exportAll: exportEmbeddingGenerations,
		insert: importMetadataEmbeddingGeneration}),
	newMetadataTable(metadataTable[metadataEmbeddingInput]{
		record: metadataEmbeddingInput{Type: metadataEmbeddingInputType}, table: "embedding_generation_inputs",
		suffix: "ORDER BY generation_id,input_order",
		validate: func(value metadataEmbeddingInput) error {
			if value.Order < 0 || value.Order >= maxEmbeddingCatalogRows {
				return errors.New("embedding input order exceeds bounds")
			}
			return nil
		}}),
	newMetadataTable(metadataTable[metadataEmbeddingVectorSet]{
		record: metadataEmbeddingVectorSet{Type: metadataEmbeddingVectorSetType}, table: "embedding_vector_sets",
		suffix: "ORDER BY vector_set_id", validate: validateMetadataEmbeddingVectorSet}),
	newMetadataTable(metadataTable[metadataEmbeddingVectorRow]{
		record: metadataEmbeddingVectorRow{Type: metadataEmbeddingVectorRowType}, table: "embedding_vector_rows",
		suffix: "ORDER BY vector_set_id,row_order",
		validate: func(value metadataEmbeddingVectorRow) error {
			if value.Order < 0 || value.Order >= maxEmbeddingCatalogRows {
				return errors.New("embedding vector row order exceeds bounds")
			}
			return nil
		}}),
	newMetadataTable(metadataTable[metadataEmbeddingSet]{
		record: metadataEmbeddingSet{Type: metadataEmbeddingSetType}, table: "embedding_sets",
		suffix: "ORDER BY embedding_set_id",
		validate: func(value metadataEmbeddingSet) error {
			return validateMetadataTime("embedding set created_at", value.CreatedAt)
		}}),
	newMetadataTable(metadataTable[metadataEmbeddingHead]{
		record: metadataEmbeddingHead{Type: metadataEmbeddingHeadType}, table: "embedding_heads",
		suffix: "ORDER BY content_version_id,profile_fingerprint,binding_id,input_kind",
		validate: func(value metadataEmbeddingHead) error {
			return validateMetadataTime("embedding head published_at", value.PublishedAt)
		}}),
	newMetadataTable(metadataTable[metadataEmbeddingFailure]{
		record: metadataEmbeddingFailure{Type: metadataEmbeddingFailureType}, table: "embedding_failures",
		suffix: "ORDER BY content_version_id,profile_fingerprint,binding_id,input_kind",
		validate: func(value metadataEmbeddingFailure) error {
			return validateMetadataTime("embedding failure failed_at", value.FailedAt)
		}}),
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

func validateMetadataEmbeddingGeneration(value metadataEmbeddingGeneration) error {
	if value.InputCount < 1 || value.InputCount > maxEmbeddingCatalogRows {
		return errors.New("embedding input count exceeds bounds")
	}
	if (value.GenerationBlobHash == "") != (value.GenerationEncodedSize == 0) ||
		value.GenerationEncodedSize < 0 || value.GenerationEncodedSize > 64<<20 {
		return errors.New("embedding generation artifact reference exceeds bounds")
	}
	return validateMetadataTime("input-generation created_at", value.CreatedAt)
}

func importMetadataEmbeddingGeneration(ctx context.Context, tx *sql.Tx, value metadataEmbeddingGeneration) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO embedding_input_generations(
		generation_id,generation_blob_hash,generation_encoded_size,generation_checksum,source_version_id,profile_fingerprint,evidence_fingerprint,
		tokenizer_fingerprint,chunk_policy_fingerprint,formatter_fingerprint,
		attachment_context_fingerprint,attachment_id,input_count,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, value.ID, nullableCatalogString(value.GenerationBlobHash), value.GenerationEncodedSize, value.GenerationChecksum, value.SourceVersionID, value.ProfileFingerprint, value.EvidenceFingerprint, value.TokenizerFingerprint, value.ChunkPolicyFingerprint, value.FormatterFingerprint, value.AttachmentContextFingerprint, value.AttachmentID, value.InputCount, value.CreatedAt)
	return err
}

func validateMetadataEmbeddingVectorSet(value metadataEmbeddingVectorSet) error {
	if value.RowCount < 1 || value.RowCount > maxEmbeddingCatalogRows {
		return errors.New("embedding vector row count exceeds bounds")
	}
	if value.PayloadSize < 1 || value.PayloadSize > 64<<20 {
		return errors.New("embedding vector payload size exceeds bounds")
	}
	return nil
}

func validateEmbeddingMetadataState(ctx context.Context, tx metadataQuerier) (retErr error) {
	headRows, err := tx.QueryContext(ctx, `SELECT content_version_id,binding_id,input_kind,
		embedding_set_id,vector_space_id,profile_fingerprint,published_at,fencing_token
		FROM embedding_heads ORDER BY content_version_id,profile_fingerprint,binding_id,input_kind`)
	if err != nil {
		return fmt.Errorf("listing embedding heads for validation: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, headRows.Close()) }()
	for headRows.Next() {
		var record EmbeddingHeadRecord
		if err := headRows.Scan(&record.Key.ContentVersionID, &record.Key.BindingID,
			&record.Key.InputKind, &record.SetID, &record.VectorSpaceID,
			&record.ProcessingProfileFingerprint, &record.PublishedAt, &record.FencingToken); err != nil {
			return fmt.Errorf("reading embedding head for validation: %w", err)
		}
		if err := validateEmbeddingHeadRecord(record); err != nil {
			return fmt.Errorf("validating embedding head: %w", err)
		}
		set, err := loadEmbeddingSetTx(ctx, tx, record.SetID)
		if err != nil {
			return err
		}
		eligible, err := embeddingAttachmentEligible(ctx, tx, set.ContentVersionID,
			set.ProcessingProfileFingerprint, set.InputGeneration.AttachmentID)
		if err != nil {
			return err
		}
		if !eligible {
			return errors.New("embedding head attachment is not current")
		}
	}
	if err := headRows.Err(); err != nil {
		return fmt.Errorf("iterating embedding heads for validation: %w", err)
	}
	if err := headRows.Close(); err != nil {
		return fmt.Errorf("closing embedding heads after validation: %w", err)
	}

	failureRows, err := tx.QueryContext(ctx, `SELECT content_version_id,profile_fingerprint,
		binding_id,input_kind,failure_code,failed_at,fencing_token,attachment_id
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
			&record.FailureCode, &record.FailedAt, &record.FencingToken, &record.AttachmentID); err != nil {
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
		if err := validateEmbeddingFailureEligibility(ctx, tx, record); err != nil {
			return fmt.Errorf("validating embedding failure eligibility: %w", err)
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
