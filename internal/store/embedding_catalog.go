package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
)

const (
	EmbeddingVectorSpaceContractV1 = "embedding-vector-space/v1"
	EmbeddingVectorSetContractV1   = "vector-set/v1"
	EmbeddingCorpusGenerationV1    = "embedding-corpus-generation/v1"

	maxEmbeddingCatalogRows    = 100_000
	maxEmbeddingCorpusMembers  = 1_000_000
	maxEmbeddingDimensions     = 1_048_576
	maxEmbeddingCatalogIDBytes = 1 << 10
	maxEmbeddingCoverageWork   = 1_000_000
	maxEmbeddingDescriptorJSON = 1 << 16
)

// EmbeddingInputKind is the canonical E1 evidence-kind vocabulary. Keep the
// alias so store callers do not need to translate a document contract token.
type EmbeddingInputKind = document.EmbeddingInputKind

// EmbeddingVectorSpaceRecord seals every identity that makes vectors mutually
// comparable. Credentials and provider request payloads are deliberately not
// part of durable catalog authority.
type EmbeddingVectorSpaceRecord struct {
	ID                    string
	ContractVersion       string
	Descriptor            document.EmbeddingDescriptor
	ProviderDescriptor    string
	ProviderRevision      string
	DescriptorFingerprint string
	CompatibilityID       string
	Dimensions            int
	Metric                string
	Normalization         string
	ScalarEncoding        string
	DocumentFormatter     string
	QueryFormatter        string
	ModelInputFingerprint string
}

// EmbeddingInputReference retains only bounded identity and rendered checksum;
// exact rendered text remains in the E2 artifact outside SQLite.
type EmbeddingInputReference struct {
	ID               string
	RenderedChecksum string
}

// EmbeddingInputGenerationRecord is the bounded catalog projection of one E2
// generation. AttachmentID fences eligibility; a non-empty context fingerprint
// additionally prevents reuse under another attachment identity.
type EmbeddingInputGenerationRecord struct {
	ID                           string
	SourceVersionID              string
	ProcessingProfileFingerprint string
	GenerationJSON               []byte
	GenerationBlobHash           string
	GenerationEncodedSize        int64
	GenerationChecksum           string
	EvidenceFingerprint          string
	TokenizerFingerprint         string
	ChunkPolicyFingerprint       string
	FormatterFingerprint         string
	AttachmentContextFingerprint string
	AttachmentID                 string
	Inputs                       []EmbeddingInputReference
	CreatedAt                    string
}

// EmbeddingVectorRowRecord binds one canonical vector-set row to one exact E2
// input without storing vector scalar values in SQLite.
type EmbeddingVectorRowRecord struct {
	RowID      string
	InputID    string
	Dimensions int
	Checksum   string
}

// EmbeddingVectorSetRecord references one canonical vector-set/v1 payload.
type EmbeddingVectorSetRecord struct {
	ID               string
	ContractVersion  string
	VectorSpaceID    string
	PayloadBlobHash  string
	PayloadSize      int64
	PayloadChecksum  string
	Payload          []byte
	ManifestChecksum string
	RowCount         int
	Dimensions       int
	Rows             []EmbeddingVectorRowRecord
}

// EmbeddingSetRecord is one immutable content/profile/binding membership.
type EmbeddingSetRecord struct {
	ID                           string
	VaultID                      string
	BindingID                    string
	InputKind                    EmbeddingInputKind
	ContentVersionID             string
	ProcessingProfileFingerprint string
	EmbeddingInputFingerprint    string
	VectorSpace                  EmbeddingVectorSpaceRecord
	InputGeneration              EmbeddingInputGenerationRecord
	VectorSet                    EmbeddingVectorSetRecord
	CreatedAt                    string
}

// EmbeddingHeadKey identifies one independently activated binding.
type EmbeddingHeadKey struct {
	ContentVersionID string
	BindingID        string
	InputKind        EmbeddingInputKind
}

// EmbeddingHeadRecord is the mutable pointer to one immutable set.
type EmbeddingHeadRecord struct {
	Key                          EmbeddingHeadKey
	SetID                        string
	VectorSpaceID                string
	ProcessingProfileFingerprint string
	PublishedAt                  string
}

// EmbeddingHead is one eligible active pointer and its immutable set.
type EmbeddingHead struct {
	Record EmbeddingHeadRecord
	Set    EmbeddingSetRecord
}

// EmbeddingFailureRecord records provider-neutral coverage failure only.
type EmbeddingFailureRecord struct {
	ContentVersionID             string
	ProcessingProfileFingerprint string
	BindingID                    string
	InputKind                    EmbeddingInputKind
	FailureCode                  EmbeddingFailureCode
	FailedAt                     string
}

// EmbeddingFailureCode is a closed provider-neutral outcome vocabulary. It is
// deliberately too small to carry provider messages, request fragments, or
// credentials into catalog metadata.
type EmbeddingFailureCode string

const (
	EmbeddingFailureProviderUnavailable EmbeddingFailureCode = "provider_unavailable"
	EmbeddingFailureAuthorization       EmbeddingFailureCode = "authorization"
	EmbeddingFailureInvalidResponse     EmbeddingFailureCode = "invalid_response"
	EmbeddingFailureInputRejected       EmbeddingFailureCode = "input_rejected"
	EmbeddingFailureStaleAuthority      EmbeddingFailureCode = "stale_authority"
)

// CoverageState distinguishes complete serving authority from partial,
// unavailable, stale, and source-ineligible states.
type CoverageState string

const (
	CoverageComplete    CoverageState = "complete"
	CoveragePartial     CoverageState = "partial"
	CoverageUnavailable CoverageState = "unavailable"
	CoverageStale       CoverageState = "stale"
	CoverageIneligible  CoverageState = "ineligible"
	CoverageRebuilding  CoverageState = "rebuilding"
)

type CoverageBinding struct {
	BindingID     string
	InputKind     EmbeddingInputKind
	VectorSpaceID string
	Required      bool
}

type CoverageScope struct {
	ContentVersionIDs            []string
	ProcessingProfileFingerprint string
	Bindings                     []CoverageBinding
}

type BindingCoverage struct {
	Binding                   CoverageBinding
	State                     CoverageState
	Complete                  int
	Unavailable               int
	Stale                     int
	Ineligible                int
	Rebuilding                int
	PreviousGenerationServing int
	Total                     int
}

type Coverage struct {
	State    CoverageState
	Required []BindingCoverage
	Optional []BindingCoverage
}

// EmbeddingCorpusGenerationRecord is a deterministic logical membership
// manifest. Rebuildable ANN files are intentionally outside this authority.
type EmbeddingCorpusGenerationRecord struct {
	ID               string
	ContractVersion  string
	BindingID        string
	VectorSpaceID    string
	SetIDs           []string
	ManifestChecksum string
	CreatedAt        string
}

// StageEmbeddingSet atomically records or exactly reuses every immutable
// authority named by a completed embedding set.
func (s *Store) StageEmbeddingSet(ctx context.Context, record EmbeddingSetRecord) error {
	record, err := s.prepareEmbeddingSetRecord(record)
	if err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return stageEmbeddingSetTx(ctx, tx, record)
	})
}

func (s *Store) prepareEmbeddingSetRecord(record EmbeddingSetRecord) (EmbeddingSetRecord, error) {
	record, err := normalizeEmbeddingSetRecord(record)
	if err != nil {
		return EmbeddingSetRecord{}, fmt.Errorf("staging embedding set: %w", err)
	}
	if err := validateEmbeddingSetRecord(record); err != nil {
		return EmbeddingSetRecord{}, fmt.Errorf("staging embedding set: %w", err)
	}
	if record.VaultID != s.vaultID {
		return EmbeddingSetRecord{}, errors.New("staging embedding set: vault identity does not match store")
	}
	return record, nil
}

func stageEmbeddingSetTx(ctx context.Context, tx *sql.Tx, record EmbeddingSetRecord) error {
	if err := validateEmbeddingSetFencesTx(ctx, tx, record); err != nil {
		return err
	}
	// Payload bytes are validated before the transaction writes catalog
	// authority. SQLite retains only the immutable blob reference and the
	// canonical row projection derived from those bytes.
	record.VectorSet.Payload = nil
	record.InputGeneration.GenerationJSON = nil
	if err := insertVectorSpaceTx(ctx, tx, record.VectorSpace); err != nil {
		return err
	}
	if err := insertInputGenerationTx(ctx, tx, record.InputGeneration); err != nil {
		return err
	}
	if err := insertVectorSetTx(ctx, tx, record.VectorSet); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO embedding_sets(
			embedding_set_id,vault_uid,binding_id,input_kind,content_version_id,
			profile_fingerprint,embedding_input_fingerprint,vector_space_id,input_generation_id,vector_set_id,created_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, record.ID, record.VaultID, record.BindingID,
		record.InputKind, record.ContentVersionID, record.ProcessingProfileFingerprint,
		record.EmbeddingInputFingerprint, record.VectorSpace.ID, record.InputGeneration.ID, record.VectorSet.ID, record.CreatedAt)
	if err != nil {
		return fmt.Errorf("inserting embedding set %s: %w", record.ID, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking embedding set insertion: %w", err)
	}
	if inserted == 0 {
		stored, err := loadEmbeddingSetTx(ctx, tx, record.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(stored, record) {
			return fmt.Errorf("embedding set %s names different immutable authority", record.ID)
		}
	}
	return nil
}

// StageEmbeddingSetWithLease stages immutable authority only while the exact
// worker lease still roots its input generation. The root and set write share
// one transaction, so an expired or superseded attempt cannot stage authority.
func (s *Store) StageEmbeddingSetWithLease(ctx context.Context, record EmbeddingSetRecord,
	rootID string, fencingToken int64, at time.Time,
) error {
	record, err := s.prepareEmbeddingSetRecord(record)
	if err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := requireEmbeddingWorkerLeaseTx(ctx, tx, rootID, fencingToken,
			record.InputGeneration.ID, at); err != nil {
			return err
		}
		return stageEmbeddingSetTx(ctx, tx, record)
	})
}

func normalizeEmbeddingSetRecord(record EmbeddingSetRecord) (EmbeddingSetRecord, error) {
	if !reflect.DeepEqual(record.VectorSpace.Descriptor, document.EmbeddingDescriptor{}) {
		descriptor, err := document.NewEmbeddingDescriptor(record.VectorSpace.Descriptor)
		if err != nil {
			return EmbeddingSetRecord{}, fmt.Errorf("invalid E1 descriptor: %w", err)
		}
		if !reflect.DeepEqual(descriptor, record.VectorSpace.Descriptor) {
			return EmbeddingSetRecord{}, errors.New("E1 descriptor is not canonical")
		}
		record.VectorSpace.ProviderDescriptor = descriptor.ID
		record.VectorSpace.ProviderRevision = descriptor.ModelRevision
		record.VectorSpace.DescriptorFingerprint = descriptor.Fingerprint
		record.VectorSpace.CompatibilityID = descriptor.CompatibilityID
		record.VectorSpace.Dimensions = descriptor.Dimension
		record.VectorSpace.Metric = descriptor.Metric
		record.VectorSpace.Normalization = descriptor.Normalization
		record.VectorSpace.ScalarEncoding = descriptor.ScalarEncoding
		record.VectorSpace.DocumentFormatter = descriptor.DocumentFormatter
		record.VectorSpace.QueryFormatter = descriptor.QueryFormatter
		record.VectorSpace.ModelInputFingerprint = descriptor.ModelInput.Fingerprint
	}
	if record.InputKind == document.EmbeddingInputRenditionChunk {
		if len(record.InputGeneration.GenerationJSON) == 0 {
			return EmbeddingSetRecord{}, errors.New("canonical E2 generation JSON is required")
		}
		generation, err := document.DecodeEmbeddingInputGeneration(record.InputGeneration.GenerationJSON, document.EmbeddingInputGenerationDecodeBounds{
			MaxEncodedBytes: 64 << 20, MaxInputs: maxEmbeddingCatalogRows,
			MaxObjectFields: 32, MaxStringBytes: 16 << 20, MaxTotalStringBytes: 64 << 20,
		})
		if err != nil {
			return EmbeddingSetRecord{}, fmt.Errorf("decoding canonical E2 generation JSON: %w", err)
		}
		if generation.Version != document.EmbeddingInputGenerationVersion {
			return EmbeddingSetRecord{}, errors.New("E2 generation artifact is not the current policy-complete version")
		}
		canonical, err := json.Marshal(generation)
		if err != nil {
			return EmbeddingSetRecord{}, fmt.Errorf("encoding canonical E2 generation JSON: %w", err)
		}
		if !bytes.Equal(canonical, record.InputGeneration.GenerationJSON) {
			return EmbeddingSetRecord{}, errors.New("E2 generation JSON is not canonical")
		}
		generationBlobHash := hashCatalogBytes(record.InputGeneration.GenerationJSON)
		if record.InputGeneration.GenerationBlobHash != "" && record.InputGeneration.GenerationBlobHash != generationBlobHash {
			return EmbeddingSetRecord{}, errors.New("E2 generation blob hash does not match exact bytes")
		}
		if record.InputGeneration.GenerationEncodedSize != 0 && record.InputGeneration.GenerationEncodedSize != int64(len(record.InputGeneration.GenerationJSON)) {
			return EmbeddingSetRecord{}, errors.New("E2 generation encoded size does not match exact bytes")
		}
		if record.InputGeneration.GenerationChecksum != "" && record.InputGeneration.GenerationChecksum != generation.Checksum {
			return EmbeddingSetRecord{}, errors.New("E2 generation checksum does not match exact artifact")
		}
		record.InputGeneration.GenerationBlobHash = generationBlobHash
		record.InputGeneration.GenerationEncodedSize = int64(len(record.InputGeneration.GenerationJSON))
		record.InputGeneration.GenerationChecksum = generation.Checksum
		expectedGenerationID := generation.Checksum
		if record.InputGeneration.AttachmentID != "" {
			expectedGenerationID = hashCatalogText("embedding-generation-attachment/v1\x00" + generation.Checksum + "\x00" + record.InputGeneration.AttachmentID)
		}
		if record.InputGeneration.ID != expectedGenerationID {
			return EmbeddingSetRecord{}, errors.New("E2 generation ID does not match its canonical checksum")
		}
		record.InputGeneration.EvidenceFingerprint = generation.EvidenceChecksum
		record.InputGeneration.TokenizerFingerprint = tokenizerCatalogFingerprint(generation.TokenizerIdentity)
		record.InputGeneration.ChunkPolicyFingerprint = generation.PolicyFingerprint
		record.InputGeneration.FormatterFingerprint = hashCatalogText(generation.Formatter)
		record.InputGeneration.AttachmentContextFingerprint = ""
		if generation.AttachmentContext != nil {
			encodedContext, marshalErr := json.Marshal(generation.AttachmentContext)
			if marshalErr != nil {
				return EmbeddingSetRecord{}, marshalErr
			}
			record.InputGeneration.AttachmentContextFingerprint = hashCatalogText(string(encodedContext))
		}
		record.InputGeneration.Inputs = make([]EmbeddingInputReference, len(generation.Inputs))
		for index, input := range generation.Inputs {
			record.InputGeneration.Inputs[index] = EmbeddingInputReference{ID: input.Key, RenderedChecksum: input.Checksum}
		}
	}
	if len(record.VectorSet.Payload) == 0 {
		return EmbeddingSetRecord{}, errors.New("canonical vector payload is required")
	}
	decoded, err := document.DecodeVectorSetV1(record.VectorSet.Payload, document.VectorBounds{
		MaxRows: maxEmbeddingCatalogRows, MaxDimension: maxEmbeddingDimensions, MaxBytes: 64 << 20,
	})
	if err != nil {
		return EmbeddingSetRecord{}, fmt.Errorf("decoding canonical vector payload: %w", err)
	}
	canonicalPayload, checksum, err := document.EncodeVectorSetV1(decoded)
	if err != nil {
		return EmbeddingSetRecord{}, fmt.Errorf("encoding canonical vector payload: %w", err)
	}
	if !bytes.Equal(canonicalPayload, record.VectorSet.Payload) {
		return EmbeddingSetRecord{}, errors.New("vector payload is not canonical vector-set/v1")
	}
	if record.VectorSet.ID != checksum || record.VectorSet.PayloadChecksum != checksum {
		return EmbeddingSetRecord{}, errors.New("vector payload checksum does not match vector-set identity")
	}
	payloadHash := hashCatalogBytes(record.VectorSet.Payload)
	if record.VectorSet.PayloadBlobHash != payloadHash {
		return EmbeddingSetRecord{}, errors.New("vector payload blob hash does not match exact bytes")
	}
	record.VectorSet.PayloadSize = int64(len(record.VectorSet.Payload))
	if record.VectorSet.VectorSpaceID != decoded.VectorSpaceFingerprint || record.VectorSpace.ID != decoded.VectorSpaceFingerprint {
		return EmbeddingSetRecord{}, errors.New("vector payload names a different vector space")
	}
	if decoded.Metric != record.VectorSpace.Metric || decoded.Normalization != record.VectorSpace.Normalization {
		return EmbeddingSetRecord{}, errors.New("vector payload metric or normalization does not match vector space")
	}
	record.VectorSet.RowCount = len(decoded.Vectors)
	record.VectorSet.Dimensions = decoded.Dimension
	canonicalRows := make([]EmbeddingVectorRowRecord, len(decoded.Vectors))
	for index := range decoded.Vectors {
		canonicalRows[index] = EmbeddingVectorRowRecord{
			RowID: decoded.InputKeys[index], InputID: decoded.InputKeys[index],
			Dimensions: decoded.Dimension, Checksum: decoded.InputChecksums[index],
		}
	}
	if record.VectorSet.Rows == nil {
		record.VectorSet.Rows = canonicalRows
		record.VectorSet.ManifestChecksum = embeddingVectorManifestChecksum(record.VectorSet.Rows)
	}
	return record, nil
}

func validateExactEmbeddingGenerationArtifact(record EmbeddingInputGenerationRecord, data []byte) error {
	if int64(len(data)) != record.GenerationEncodedSize || hashCatalogBytes(data) != record.GenerationBlobHash {
		return errors.New("E2 generation artifact bytes do not match catalog authority")
	}
	generation, err := document.DecodeEmbeddingInputGeneration(data, document.EmbeddingInputGenerationDecodeBounds{
		MaxEncodedBytes: record.GenerationEncodedSize, MaxInputs: maxEmbeddingCatalogRows,
		MaxObjectFields: 32, MaxStringBytes: min(record.GenerationEncodedSize, 16<<20),
		MaxTotalStringBytes: record.GenerationEncodedSize,
	})
	if err != nil {
		return err
	}
	if generation.Version != document.EmbeddingInputGenerationVersion {
		return errors.New("E2 generation artifact is not the current policy-complete version")
	}
	canonical, err := json.Marshal(generation)
	if err != nil || !bytes.Equal(canonical, data) {
		return errors.New("E2 generation artifact is not canonical")
	}
	expectedID := generation.Checksum
	if record.AttachmentID != "" {
		expectedID = hashCatalogText("embedding-generation-attachment/v1\x00" + generation.Checksum + "\x00" + record.AttachmentID)
	}
	if record.ID != expectedID || record.GenerationChecksum != generation.Checksum ||
		record.EvidenceFingerprint != generation.EvidenceChecksum ||
		record.TokenizerFingerprint != tokenizerCatalogFingerprint(generation.TokenizerIdentity) ||
		record.ChunkPolicyFingerprint != generation.PolicyFingerprint ||
		record.FormatterFingerprint != hashCatalogText(generation.Formatter) {
		return errors.New("E2 generation catalog projection does not match exact artifact")
	}
	contextFingerprint := ""
	if generation.AttachmentContext != nil {
		encoded, marshalErr := json.Marshal(generation.AttachmentContext)
		if marshalErr != nil {
			return marshalErr
		}
		contextFingerprint = hashCatalogBytes(encoded)
	}
	if record.AttachmentContextFingerprint != contextFingerprint || len(record.Inputs) != len(generation.Inputs) {
		return errors.New("E2 generation context or input count does not match exact artifact")
	}
	for index, input := range generation.Inputs {
		if record.Inputs[index] != (EmbeddingInputReference{ID: input.Key, RenderedChecksum: input.Checksum}) {
			return errors.New("E2 generation inputs do not match exact artifact")
		}
	}
	return nil
}

// HydrateEmbeddingInputGeneration binds an exact canonical E2 artifact to its
// catalog projection. The caller may stage only the returned record.
func HydrateEmbeddingInputGeneration(record EmbeddingInputGenerationRecord, data []byte) (EmbeddingInputGenerationRecord, error) {
	if err := validateExactEmbeddingGenerationArtifact(record, data); err != nil {
		return EmbeddingInputGenerationRecord{}, fmt.Errorf("hydrating exact artifact: %w", err)
	}
	record.GenerationJSON = bytes.Clone(data)
	return record, nil
}

func validateExactEmbeddingVectorArtifact(
	vectorSet EmbeddingVectorSetRecord, space EmbeddingVectorSpaceRecord,
	generation EmbeddingInputGenerationRecord, data []byte,
) error {
	if int64(len(data)) != vectorSet.PayloadSize || hashCatalogBytes(data) != vectorSet.PayloadBlobHash {
		return errors.New("vector-set artifact bytes do not match catalog authority")
	}
	decoded, err := document.DecodeVectorSetV1(data, document.VectorBounds{
		MaxRows: maxEmbeddingCatalogRows, MaxDimension: maxEmbeddingDimensions, MaxBytes: len(data),
	})
	if err != nil {
		return err
	}
	canonical, checksum, err := document.EncodeVectorSetV1(decoded)
	if err != nil || !bytes.Equal(canonical, data) {
		return errors.New("vector-set artifact is not canonical")
	}
	if vectorSet.ID != checksum || vectorSet.PayloadChecksum != checksum ||
		decoded.VectorSpaceFingerprint != space.ID || decoded.Dimension != space.Dimensions ||
		decoded.Metric != space.Metric || decoded.Normalization != space.Normalization ||
		len(decoded.InputKeys) != len(generation.Inputs) || len(vectorSet.Rows) != len(generation.Inputs) {
		return errors.New("vector-set artifact header or count does not match catalog authority")
	}
	for index := range generation.Inputs {
		if decoded.InputKeys[index] != generation.Inputs[index].ID ||
			decoded.InputChecksums[index] != generation.Inputs[index].RenderedChecksum ||
			vectorSet.Rows[index] != (EmbeddingVectorRowRecord{
				RowID: decoded.InputKeys[index], InputID: decoded.InputKeys[index],
				Dimensions: decoded.Dimension, Checksum: decoded.InputChecksums[index],
			}) {
			return errors.New("vector-set artifact rows do not match exact E2 inputs")
		}
	}
	if vectorSet.ManifestChecksum != embeddingVectorManifestChecksum(vectorSet.Rows) {
		return errors.New("vector-set artifact manifest checksum does not match rows")
	}
	return nil
}

func tokenizerCatalogFingerprint(identity document.TokenizerIdentity) string {
	return hashCatalogText(identity.Name + "\x00" + identity.Revision + "\x00" + strconv.FormatBool(identity.PrefixTokenCountsMonotonic))
}

func hashCatalogText(value string) string { return hashCatalogBytes([]byte(value)) }

func hashCatalogBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func nullableCatalogString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// PublishEmbeddingHead activates exactly one binding without changing any
// other chunk/direct-file head or the independent rendition head.
func (s *Store) PublishEmbeddingHead(ctx context.Context, record EmbeddingHeadRecord) error {
	if err := validateEmbeddingHeadRecord(record); err != nil {
		return fmt.Errorf("publishing embedding head: %w", err)
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return publishEmbeddingHeadTx(ctx, tx, record)
	})
}

func publishEmbeddingHeadTx(ctx context.Context, tx *sql.Tx, record EmbeddingHeadRecord) error {
	set, err := loadEmbeddingSetTx(ctx, tx, record.SetID)
	if err != nil {
		return fmt.Errorf("publishing embedding head: %w", err)
	}
	if set.ContentVersionID != record.Key.ContentVersionID || set.BindingID != record.Key.BindingID ||
		set.InputKind != record.Key.InputKind || set.VectorSpace.ID != record.VectorSpaceID ||
		set.ProcessingProfileFingerprint != record.ProcessingProfileFingerprint {
		return errors.New("publishing embedding head: stale source, profile, binding, or vector-space fence")
	}
	eligible, err := embeddingSetEligibleTx(ctx, tx, set)
	if err != nil {
		return err
	}
	if !eligible {
		return errors.New("publishing embedding head: source or attachment is not eligible")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO embedding_heads(
			content_version_id,binding_id,input_kind,embedding_set_id,vector_space_id,
			profile_fingerprint,published_at
		) VALUES(?,?,?,?,?,?,?) ON CONFLICT(content_version_id,binding_id,input_kind)
		DO UPDATE SET embedding_set_id=excluded.embedding_set_id,
		vector_space_id=excluded.vector_space_id,profile_fingerprint=excluded.profile_fingerprint,
		published_at=excluded.published_at`, record.Key.ContentVersionID, record.Key.BindingID,
		record.Key.InputKind, record.SetID, record.VectorSpaceID,
		record.ProcessingProfileFingerprint, record.PublishedAt)
	if err != nil {
		return fmt.Errorf("publishing embedding head: %w", err)
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM embedding_failures
			WHERE content_version_id=? AND binding_id=? AND input_kind=?`,
		record.Key.ContentVersionID, record.Key.BindingID, record.Key.InputKind)
	return err
}

// PublishEmbeddingHeadWithLease atomically rechecks the exact consent receipt,
// worker lease, set membership, source/attachment eligibility, and head flip.
func (s *Store) PublishEmbeddingHeadWithLease(ctx context.Context, record EmbeddingHeadRecord,
	consent ProviderOperationAuthorizationRequest, prior ProviderOperationAuthorization,
	rootID string, fencingToken int64, at time.Time,
) (ProviderOperationAuthorization, error) {
	if err := validateEmbeddingHeadRecord(record); err != nil {
		return ProviderOperationAuthorization{}, fmt.Errorf("publishing leased embedding head: %w", err)
	}
	consent.PriorAuthorization = &prior
	var authorization ProviderOperationAuthorization
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		set, err := loadEmbeddingSetTx(ctx, tx, record.SetID)
		if err != nil {
			return err
		}
		if err := requireEmbeddingWorkerLeaseTx(ctx, tx, rootID, fencingToken,
			set.InputGeneration.ID, at); err != nil {
			return err
		}
		authorization, err = authorizeProviderOperationTx(ctx, tx, s.vaultID, consent, at.UTC())
		if err != nil {
			return err
		}
		return publishEmbeddingHeadTx(ctx, tx, record)
	})
	return authorization, err
}

func requireEmbeddingWorkerLeaseTx(ctx context.Context, tx *sql.Tx, rootID string,
	fencingToken int64, generationID string, at time.Time,
) error {
	if rootID == "" || fencingToken <= 0 || generationID == "" || at.IsZero() {
		return ErrCurrentRenditionRootFenced
	}
	var expiresAt string
	err := tx.QueryRowContext(ctx, `SELECT expires_at FROM current_rendition_roots
		WHERE root_id=? AND root_kind=? AND target_kind=? AND target_id=?
		  AND fencing_token=? AND active=1`, rootID, RenditionRootWorkerLease,
		RenditionRootEmbeddingGeneration, generationID, fencingToken).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCurrentRenditionRootFenced
	}
	if err != nil {
		return fmt.Errorf("checking embedding worker lease: %w", err)
	}
	expires, err := time.Parse(timestampLayout, expiresAt)
	if err != nil || !expires.After(at.UTC()) {
		return ErrCurrentRenditionRootFenced
	}
	return nil
}

// ActiveEmbeddingHead returns only a head whose exact content version remains
// live and current and whose chunk attachment remains the active rendition.
func (s *Store) ActiveEmbeddingHead(ctx context.Context, key EmbeddingHeadKey) (EmbeddingHead, error) {
	if err := validateEmbeddingHeadKey(key); err != nil {
		return EmbeddingHead{}, err
	}
	var record EmbeddingHeadRecord
	var loaded EmbeddingHead
	record.Key = key
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT embedding_set_id,vector_space_id,
			profile_fingerprint,published_at FROM embedding_heads
			WHERE content_version_id=? AND binding_id=? AND input_kind=?`, key.ContentVersionID,
			key.BindingID, key.InputKind).Scan(&record.SetID, &record.VectorSpaceID,
			&record.ProcessingProfileFingerprint, &record.PublishedAt); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("reading embedding head: %w", err)
		}
		set, err := loadEmbeddingSetTx(ctx, tx, record.SetID)
		if err != nil {
			return err
		}
		eligible, err := embeddingSetEligibleTx(ctx, tx, set)
		if err != nil {
			return err
		}
		if !eligible || set.VectorSpace.ID != record.VectorSpaceID ||
			set.ProcessingProfileFingerprint != record.ProcessingProfileFingerprint {
			return ErrNotFound
		}
		loaded = EmbeddingHead{Record: record, Set: set}
		return nil
	})
	return loaded, err
}

// RecordEmbeddingFailure records provider-neutral plan status without
// disturbing any rendition or sibling embedding head.
func (s *Store) RecordEmbeddingFailure(ctx context.Context, record EmbeddingFailureRecord) error {
	if err := validateEmbeddingFailureRecord(record); err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		binding, _, err := embeddingProfileBindingAuthority(ctx, tx, record.ProcessingProfileFingerprint, record.BindingID)
		if err != nil {
			return err
		}
		if binding.InputKind != record.InputKind {
			return errors.New("embedding failure does not match processing profile binding kind")
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM content_versions v
			JOIN processing_profiles p ON p.profile_fingerprint=? WHERE v.version_id=?`,
			record.ProcessingProfileFingerprint, record.ContentVersionID).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO embedding_failures(
			content_version_id,profile_fingerprint,binding_id,input_kind,failure_code,failed_at
		) VALUES(?,?,?,?,?,?) ON CONFLICT(content_version_id,binding_id,input_kind)
		DO UPDATE SET profile_fingerprint=excluded.profile_fingerprint,
		failure_code=excluded.failure_code,failed_at=excluded.failed_at`, record.ContentVersionID,
			record.ProcessingProfileFingerprint, record.BindingID, record.InputKind,
			record.FailureCode, record.FailedAt)
		return err
	})
}

// EmbeddingCoverage reports each required and optional binding independently.
func (s *Store) EmbeddingCoverage(ctx context.Context, scope CoverageScope) (Coverage, error) {
	if err := validateCoverageScope(scope); err != nil {
		return Coverage{}, err
	}
	for _, requested := range scope.Bindings {
		binding, fingerprints, err := embeddingProfileBindingAuthority(ctx, s.db, scope.ProcessingProfileFingerprint, requested.BindingID)
		if err != nil {
			return Coverage{}, err
		}
		if binding.InputKind != requested.InputKind ||
			(binding.Activation == document.EmbeddingRequired) != requested.Required ||
			(requested.VectorSpaceID != "" && requested.VectorSpaceID != fingerprints.VectorSpace[binding.Name]) {
			return Coverage{}, errors.New("coverage binding does not match processing profile authority")
		}
	}
	coverage := Coverage{State: CoverageComplete}
	for _, binding := range scope.Bindings {
		item := BindingCoverage{Binding: binding, Total: len(scope.ContentVersionIDs)}
		for _, versionID := range scope.ContentVersionIDs {
			state, err := s.embeddingCoverageState(ctx, versionID, scope.ProcessingProfileFingerprint, binding)
			if err != nil {
				return Coverage{}, err
			}
			switch state {
			case CoverageComplete:
				item.Complete++
			case CoverageUnavailable:
				item.Unavailable++
			case CoverageStale:
				item.Stale++
			case CoverageIneligible:
				item.Ineligible++
			case CoverageRebuilding:
				item.Rebuilding++
			case CoveragePartial:
				return Coverage{}, errors.New("single-version embedding coverage cannot be partial")
			}
		}
		item.State = summarizeBindingCoverage(item)
		if binding.Required {
			coverage.Required = append(coverage.Required, item)
		} else {
			coverage.Optional = append(coverage.Optional, item)
		}
	}
	coverage.State = summarizeCoverage(coverage)
	return coverage, nil
}

func embeddingProfileBindingAuthority(ctx context.Context, query metadataQuerier, profileFingerprint, bindingID string) (document.EmbeddingBindingV1, document.FingerprintSet, error) {
	record, err := loadProcessingProfile(ctx, query, profileFingerprint)
	if err != nil {
		return document.EmbeddingBindingV1{}, document.FingerprintSet{}, fmt.Errorf("embedding processing profile: %w", err)
	}
	var profile document.ProcessingProfileV1
	if err := json.Unmarshal(record.CanonicalProfile, &profile); err != nil {
		return document.EmbeddingBindingV1{}, document.FingerprintSet{}, fmt.Errorf("decoding embedding processing profile: %w", err)
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	if err != nil || !bytes.Equal(canonical, record.CanonicalProfile) || fingerprints.Profile != profileFingerprint {
		return document.EmbeddingBindingV1{}, document.FingerprintSet{}, errors.New("embedding processing profile is not canonical")
	}
	for _, binding := range profile.Embeddings {
		if binding.Name == bindingID {
			return binding, fingerprints, nil
		}
	}
	return document.EmbeddingBindingV1{}, document.FingerprintSet{}, fmt.Errorf("processing profile binding %q does not exist", bindingID)
}

// StageEmbeddingCorpusGeneration stores one sorted logical corpus manifest.
func (s *Store) StageEmbeddingCorpusGeneration(ctx context.Context, record EmbeddingCorpusGenerationRecord) error {
	if err := validateCatalogSHA256(record.ID, "embedding corpus generation ID"); err != nil {
		return err
	}
	if record.ContractVersion != EmbeddingCorpusGenerationV1 {
		return errors.New("embedding corpus generation contract version is unsupported")
	}
	if err := validateEmbeddingCatalogText(record.BindingID, "embedding corpus binding"); err != nil {
		return err
	}
	if err := validateCatalogSHA256(record.VectorSpaceID, "embedding corpus vector-space ID"); err != nil {
		return err
	}
	if err := validateCatalogSHA256(record.ManifestChecksum, "embedding corpus manifest checksum"); err != nil {
		return err
	}
	if err := validateMetadataTime("embedding corpus created_at", record.CreatedAt); err != nil {
		return err
	}
	sorted, err := sortedCorpusSetIDs(record.SetIDs)
	if err != nil {
		return err
	}
	if embeddingCorpusManifestChecksum(sorted) != record.ManifestChecksum {
		return errors.New("embedding corpus manifest checksum mismatch")
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for _, setID := range sorted {
			set, err := loadEmbeddingSetTx(ctx, tx, setID)
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("embedding corpus set %s: %w", setID, ErrNotFound)
			} else if err != nil {
				return err
			}
			if set.BindingID != record.BindingID || set.VectorSpace.ID != record.VectorSpaceID {
				return errors.New("embedding corpus contains a different binding or vector space")
			}
			var activeSetID string
			if err := tx.QueryRowContext(ctx, `SELECT embedding_set_id FROM embedding_heads
				WHERE content_version_id=? AND binding_id=? AND input_kind=?`, set.ContentVersionID,
				set.BindingID, set.InputKind).Scan(&activeSetID); errors.Is(err, sql.ErrNoRows) {
				return errors.New("embedding corpus contains an unpublished or stale set")
			} else if err != nil {
				return err
			}
			eligible, err := embeddingSetEligibleTx(ctx, tx, set)
			if err != nil {
				return err
			}
			if activeSetID != set.ID || !eligible {
				return errors.New("embedding corpus contains an unpublished, stale, or ineligible set")
			}
		}
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO embedding_corpus_generations(
			corpus_generation_id,contract_version,binding_id,vector_space_id,
			manifest_checksum,member_count,created_at) VALUES(?,?,?,?,?,?,?)`, record.ID,
			record.ContractVersion, record.BindingID, record.VectorSpaceID,
			record.ManifestChecksum, len(sorted), record.CreatedAt)
		if err != nil {
			return fmt.Errorf("inserting embedding corpus generation: %w", err)
		}
		inserted, _ := result.RowsAffected()
		if inserted == 0 {
			stored, err := loadEmbeddingCorpusGenerationTx(ctx, tx, record.ID)
			if err != nil {
				return err
			}
			expected := record
			expected.SetIDs = sorted
			if !reflect.DeepEqual(stored, expected) {
				return errors.New("embedding corpus generation names different immutable authority")
			}
			return nil
		}
		for order, setID := range sorted {
			if _, err := tx.ExecContext(ctx, `INSERT INTO embedding_corpus_members(
				corpus_generation_id,member_order,embedding_set_id) VALUES(?,?,?)`,
				record.ID, order, setID); err != nil {
				return fmt.Errorf("inserting embedding corpus member: %w", err)
			}
		}
		return nil
	})
}

func loadEmbeddingCorpusGenerationTx(ctx context.Context, tx *sql.Tx, id string) (EmbeddingCorpusGenerationRecord, error) {
	var record EmbeddingCorpusGenerationRecord
	var count int
	err := tx.QueryRowContext(ctx, `SELECT corpus_generation_id,contract_version,binding_id,
		vector_space_id,manifest_checksum,member_count,created_at
		FROM embedding_corpus_generations WHERE corpus_generation_id=?`, id).Scan(&record.ID,
		&record.ContractVersion, &record.BindingID, &record.VectorSpaceID,
		&record.ManifestChecksum, &count, &record.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	if err != nil {
		return record, err
	}
	if count < 0 || count > maxEmbeddingCorpusMembers {
		return record, errors.New("embedding corpus member count is corrupt")
	}
	rows, err := tx.QueryContext(ctx, `SELECT embedding_set_id FROM embedding_corpus_members
		WHERE corpus_generation_id=? ORDER BY member_order`, id)
	if err != nil {
		return record, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var setID string
		if err := rows.Scan(&setID); err != nil {
			return record, err
		}
		record.SetIDs = append(record.SetIDs, setID)
		if len(record.SetIDs) > count {
			return record, errors.New("embedding corpus members exceed declared count")
		}
	}
	if err := rows.Err(); err != nil {
		return record, err
	}
	if len(record.SetIDs) != count || embeddingCorpusManifestChecksum(record.SetIDs) != record.ManifestChecksum {
		return record, errors.New("embedding corpus manifest is corrupt")
	}
	return record, nil
}

func embeddingCorpusManifestChecksum(setIDs []string) string {
	hash := sha256.New()
	for _, setID := range setIDs {
		_, _ = hash.Write([]byte(setID))
		_, _ = hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (s *Store) embeddingCoverageState(ctx context.Context, versionID, profile string, binding CoverageBinding) (CoverageState, error) {
	var eligible int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM content_versions v
		JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id AND n.trashed_at IS NULL
		WHERE v.version_id=?`, versionID).Scan(&eligible); err != nil {
		return "", err
	}
	if eligible == 0 {
		return CoverageIneligible, nil
	}
	head, err := s.ActiveEmbeddingHead(ctx, EmbeddingHeadKey{
		ContentVersionID: versionID, BindingID: binding.BindingID, InputKind: binding.InputKind,
	})
	if err == nil {
		if binding.VectorSpaceID != "" && head.Record.VectorSpaceID != binding.VectorSpaceID ||
			head.Record.ProcessingProfileFingerprint != profile {
			return CoverageStale, nil
		}
		return CoverageComplete, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	var headedSetID string
	err = s.db.QueryRowContext(ctx, `SELECT embedding_set_id FROM embedding_heads
		WHERE content_version_id=? AND binding_id=? AND input_kind=?`, versionID,
		binding.BindingID, binding.InputKind).Scan(&headedSetID)
	if err == nil {
		return CoverageIneligible, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var failures int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embedding_failures
		WHERE content_version_id=? AND profile_fingerprint=? AND binding_id=? AND input_kind=?`,
		versionID, profile, binding.BindingID, binding.InputKind).Scan(&failures); err != nil {
		return "", err
	}
	if failures != 0 {
		return CoverageUnavailable, nil
	}
	return CoverageUnavailable, nil
}

func validateEmbeddingSetRecord(record EmbeddingSetRecord) error {
	if err := validateCatalogSHA256(record.ID, "embedding set ID"); err != nil {
		return err
	}
	if err := validateUUIDv4(record.VaultID); err != nil {
		return fmt.Errorf("embedding vault ID: %w", err)
	}
	if err := validateUUIDv4(record.ContentVersionID); err != nil {
		return fmt.Errorf("embedding content version ID: %w", err)
	}
	if err := validateCatalogSHA256(record.ProcessingProfileFingerprint, "embedding profile fingerprint"); err != nil {
		return err
	}
	if err := validateEmbeddingCatalogText(record.BindingID, "embedding binding ID"); err != nil {
		return err
	}
	if err := validateEmbeddingInputKind(record.InputKind); err != nil {
		return err
	}
	if err := validateEmbeddingVectorSpace(record.VectorSpace); err != nil {
		return err
	}
	if err := validateEmbeddingInputGeneration(record.InputGeneration); err != nil {
		return err
	}
	if err := validateEmbeddingVectorSet(record.VectorSet, record.VectorSpace, record.InputGeneration); err != nil {
		return err
	}
	if record.InputGeneration.SourceVersionID != record.ContentVersionID ||
		record.InputGeneration.ProcessingProfileFingerprint != record.ProcessingProfileFingerprint {
		return errors.New("embedding generation source/profile membership does not match set")
	}
	return validateMetadataTime("embedding set created_at", record.CreatedAt)
}

func validateEmbeddingVectorSpace(record EmbeddingVectorSpaceRecord) error {
	if record.ContractVersion != EmbeddingVectorSpaceContractV1 {
		return errors.New("embedding vector-space contract version is unsupported")
	}
	for value, subject := range map[string]string{
		record.ID: "vector-space ID", record.DescriptorFingerprint: "descriptor fingerprint",
		record.ModelInputFingerprint: "model-input fingerprint",
	} {
		if err := validateCatalogSHA256(value, subject); err != nil {
			return err
		}
	}
	for value, subject := range map[string]string{
		record.ProviderDescriptor: "provider descriptor", record.ProviderRevision: "provider revision",
		record.CompatibilityID: "compatibility ID", record.Metric: "vector metric",
		record.Normalization: "vector normalization", record.ScalarEncoding: "scalar encoding",
		record.DocumentFormatter: "document formatter", record.QueryFormatter: "query formatter",
	} {
		if err := validateEmbeddingCatalogText(value, subject); err != nil {
			return err
		}
	}
	if record.Dimensions < 1 || record.Dimensions > maxEmbeddingDimensions {
		return errors.New("embedding vector-space dimensions are invalid")
	}
	if err := validateEmbeddingVectorSpaceProjection(record); err != nil {
		return err
	}
	return nil
}

func validateEmbeddingVectorSpaceProjection(record EmbeddingVectorSpaceRecord) error {
	descriptor, err := document.NewEmbeddingDescriptor(record.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, record.Descriptor) {
		return errors.New("embedding vector-space E1 descriptor is not canonical")
	}
	if record.ProviderDescriptor != descriptor.ID || record.ProviderRevision != descriptor.ModelRevision ||
		record.DescriptorFingerprint != descriptor.Fingerprint || record.CompatibilityID != descriptor.CompatibilityID ||
		record.Dimensions != descriptor.Dimension || record.Metric != descriptor.Metric ||
		record.Normalization != descriptor.Normalization || record.ScalarEncoding != descriptor.ScalarEncoding ||
		record.DocumentFormatter != descriptor.DocumentFormatter || record.QueryFormatter != descriptor.QueryFormatter ||
		record.ModelInputFingerprint != descriptor.ModelInput.Fingerprint {
		return errors.New("embedding vector-space projection does not match canonical E1 descriptor")
	}
	return nil
}

func validateEmbeddingInputGeneration(record EmbeddingInputGenerationRecord) error {
	if err := validateUUIDv4(record.SourceVersionID); err != nil {
		return fmt.Errorf("source version ID: %w", err)
	}
	for value, subject := range map[string]string{
		record.ID:                           "input-generation ID",
		record.ProcessingProfileFingerprint: "input-generation profile fingerprint",
		record.EvidenceFingerprint:          "evidence fingerprint", record.TokenizerFingerprint: "tokenizer fingerprint",
		record.ChunkPolicyFingerprint: "chunk policy fingerprint", record.FormatterFingerprint: "formatter fingerprint",
		record.GenerationChecksum: "input-generation checksum",
	} {
		if err := validateCatalogSHA256(value, subject); err != nil {
			return err
		}
	}
	if record.GenerationBlobHash != "" {
		if err := validateCatalogSHA256(record.GenerationBlobHash, "input-generation blob hash"); err != nil {
			return err
		}
		if record.GenerationEncodedSize < 2 || record.GenerationEncodedSize > 64<<20 {
			return errors.New("input-generation encoded size is invalid")
		}
	} else if record.GenerationEncodedSize != 0 {
		return errors.New("input-generation blob size requires a blob hash")
	}
	if record.AttachmentContextFingerprint != "" {
		if err := validateCatalogSHA256(record.AttachmentContextFingerprint, "attachment-context fingerprint"); err != nil {
			return err
		}
		if record.AttachmentID == "" {
			return errors.New("attachment context requires an attachment identity")
		}
	}
	if record.AttachmentID != "" {
		if err := validateCatalogSHA256(record.AttachmentID, "embedding attachment ID"); err != nil {
			return err
		}
	}
	if len(record.Inputs) < 1 || len(record.Inputs) > maxEmbeddingCatalogRows {
		return errors.New("embedding input generation count is invalid")
	}
	seen := make(map[string]struct{}, len(record.Inputs))
	for _, input := range record.Inputs {
		if err := validateEmbeddingCatalogText(input.ID, "embedding input ID"); err != nil {
			return err
		}
		if err := validateCatalogSHA256(input.RenderedChecksum, "rendered input checksum"); err != nil {
			return err
		}
		if _, duplicate := seen[input.ID]; duplicate {
			return errors.New("embedding input generation contains duplicate input IDs")
		}
		seen[input.ID] = struct{}{}
	}
	return validateMetadataTime("input-generation created_at", record.CreatedAt)
}

func validateEmbeddingVectorSet(vectorSet EmbeddingVectorSetRecord, space EmbeddingVectorSpaceRecord, generation EmbeddingInputGenerationRecord) error {
	if vectorSet.ContractVersion != EmbeddingVectorSetContractV1 {
		return errors.New("embedding vector-set contract version is unsupported")
	}
	for value, subject := range map[string]string{
		vectorSet.ID: "vector-set ID", vectorSet.PayloadBlobHash: "vector payload blob hash",
		vectorSet.PayloadChecksum: "vector payload checksum", vectorSet.ManifestChecksum: "vector manifest checksum",
		vectorSet.VectorSpaceID: "vector-set vector-space ID",
	} {
		if err := validateCatalogSHA256(value, subject); err != nil {
			return err
		}
	}
	if vectorSet.RowCount != len(vectorSet.Rows) || vectorSet.RowCount != len(generation.Inputs) ||
		vectorSet.RowCount < 1 || vectorSet.RowCount > maxEmbeddingCatalogRows {
		return errors.New("embedding vector-set row count does not match input generation")
	}
	if vectorSet.ID != vectorSet.PayloadChecksum {
		return errors.New("embedding vector-set ID does not match payload checksum")
	}
	if vectorSet.Dimensions != space.Dimensions || vectorSet.VectorSpaceID != space.ID {
		return errors.New("embedding vector-set dimensions do not match vector space")
	}
	if vectorSet.PayloadSize < 1 || vectorSet.PayloadSize > 64<<20 {
		return errors.New("embedding vector payload size is invalid")
	}
	inputs := make(map[string]struct{}, len(generation.Inputs))
	for _, input := range generation.Inputs {
		inputs[input.ID] = struct{}{}
	}
	seenRows := make(map[string]struct{}, len(vectorSet.Rows))
	seenInputs := make(map[string]struct{}, len(vectorSet.Rows))
	for index, row := range vectorSet.Rows {
		if err := validateEmbeddingCatalogText(row.RowID, "embedding vector row ID"); err != nil {
			return err
		}
		if err := validateCatalogSHA256(row.Checksum, "embedding vector row checksum"); err != nil {
			return err
		}
		if row.Dimensions != space.Dimensions {
			return errors.New("embedding vector row dimension mismatch")
		}
		if _, expected := inputs[row.InputID]; !expected {
			return errors.New("embedding vector set contains an unexpected input row")
		}
		if _, duplicate := seenRows[row.RowID]; duplicate {
			return errors.New("embedding vector set contains duplicate row IDs")
		}
		if _, duplicate := seenInputs[row.InputID]; duplicate {
			return errors.New("embedding vector set contains duplicate input rows")
		}
		if row.RowID != row.InputID {
			return errors.New("embedding vector row ID does not match canonical input key")
		}
		if row.InputID != generation.Inputs[index].ID || row.Checksum != generation.Inputs[index].RenderedChecksum {
			return errors.New("embedding vector rows do not match ordered generation inputs and checksums")
		}
		seenRows[row.RowID], seenInputs[row.InputID] = struct{}{}, struct{}{}
	}
	if embeddingVectorManifestChecksum(vectorSet.Rows) != vectorSet.ManifestChecksum {
		return errors.New("embedding vector-set manifest checksum mismatch")
	}
	return nil
}

func embeddingVectorManifestChecksum(rows []EmbeddingVectorRowRecord) string {
	hash := sha256.New()
	for _, row := range rows {
		_, _ = hash.Write([]byte(row.RowID))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(row.InputID))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(strconv.Itoa(row.Dimensions)))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(row.Checksum))
		_, _ = hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func validateEmbeddingSetFencesTx(ctx context.Context, tx *sql.Tx, record EmbeddingSetRecord) error {
	profileRecord, err := loadProcessingProfile(ctx, tx, record.ProcessingProfileFingerprint)
	if err != nil {
		return fmt.Errorf("embedding set processing profile: %w", err)
	}
	var profile document.ProcessingProfileV1
	if err := json.Unmarshal(profileRecord.CanonicalProfile, &profile); err != nil {
		return fmt.Errorf("decoding embedding processing profile: %w", err)
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	if err != nil || !bytes.Equal(canonical, profileRecord.CanonicalProfile) || fingerprints.Profile != record.ProcessingProfileFingerprint {
		return errors.New("embedding processing profile authority is not canonical")
	}
	var binding *document.EmbeddingBindingV1
	for index := range profile.Embeddings {
		if profile.Embeddings[index].Name == record.BindingID {
			binding = &profile.Embeddings[index]
			break
		}
	}
	if binding == nil {
		return fmt.Errorf("processing profile binding %q does not exist", record.BindingID)
	}
	if err := validateEmbeddingBindingAuthority(record, *binding, fingerprints); err != nil {
		return err
	}
	var versionCount, blobCount, blobSize int64
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM content_versions WHERE version_id=?),
		(SELECT COUNT(*) FROM blobs WHERE hash=?),
		COALESCE((SELECT size FROM blobs WHERE hash=?),-1)`, record.ContentVersionID,
		record.VectorSet.PayloadBlobHash, record.VectorSet.PayloadBlobHash).Scan(&versionCount, &blobCount, &blobSize); err != nil {
		return err
	}
	if versionCount != 1 || blobCount != 1 || blobSize != int64(len(record.VectorSet.Payload)) {
		return errors.New("embedding set references a missing source version or exact vector payload")
	}
	if record.InputKind == document.EmbeddingInputRenditionChunk {
		var generationBlobCount, generationBlobSize int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(size),-1) FROM blobs WHERE hash=?`,
			record.InputGeneration.GenerationBlobHash).Scan(&generationBlobCount, &generationBlobSize); err != nil {
			return err
		}
		if generationBlobCount != 1 || generationBlobSize != int64(len(record.InputGeneration.GenerationJSON)) {
			return errors.New("embedding set references a missing exact E2 generation artifact")
		}
	}
	if record.InputKind == document.EmbeddingInputRenditionChunk && record.InputGeneration.AttachmentID == "" {
		return errors.New("chunk embedding set requires rendition attachment authority")
	}
	if record.InputKind == document.EmbeddingInputOriginalFile {
		if record.InputGeneration.AttachmentID != "" || len(record.InputGeneration.GenerationJSON) != 0 ||
			record.InputGeneration.GenerationBlobHash != "" || record.InputGeneration.GenerationEncodedSize != 0 ||
			len(record.InputGeneration.Inputs) != 1 {
			return errors.New("original-file input authority cannot claim rendition attachment or E2 generation")
		}
		var sourceHash string
		if err := tx.QueryRowContext(ctx, `SELECT blob_hash FROM content_versions WHERE version_id=?`, record.ContentVersionID).Scan(&sourceHash); err != nil {
			return err
		}
		if record.InputGeneration.GenerationChecksum != sourceHash ||
			record.InputGeneration.Inputs[0] != (EmbeddingInputReference{ID: record.ContentVersionID, RenderedChecksum: sourceHash}) {
			return errors.New("original-file input authority must name the exact source version bytes")
		}
	} else if record.InputGeneration.AttachmentID != "" {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rendition_attachments
			WHERE attachment_id=? AND content_version_id=? AND profile_fingerprint=?`,
			record.InputGeneration.AttachmentID, record.ContentVersionID,
			record.ProcessingProfileFingerprint).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return errors.New("embedding generation attachment fence does not match source/profile")
		}
		var evidenceChecksum, sourceHash string
		if err := tx.QueryRowContext(ctx, `SELECT b.evidence_checksum,b.source_sha256
			FROM rendition_attachments a JOIN rendition_builds b ON b.build_id=a.build_id
			WHERE a.attachment_id=?`, record.InputGeneration.AttachmentID).Scan(&evidenceChecksum, &sourceHash); err != nil {
			return err
		}
		var generation document.EmbeddingInputGeneration
		if err := json.Unmarshal(record.InputGeneration.GenerationJSON, &generation); err != nil {
			return err
		}
		if generation.EvidenceChecksum != evidenceChecksum {
			return errors.New("E2 generation evidence checksum does not match attachment authority")
		}
		var contentHash string
		if err := tx.QueryRowContext(ctx, `SELECT blob_hash FROM content_versions WHERE version_id=?`, record.ContentVersionID).Scan(&contentHash); err != nil {
			return err
		}
		if contentHash != sourceHash {
			return errors.New("E2 attachment source does not match content version")
		}
	}
	return nil
}

func validateEmbeddingBindingAuthority(record EmbeddingSetRecord, binding document.EmbeddingBindingV1, fingerprints document.FingerprintSet) error {
	space := record.VectorSpace
	if record.InputKind != binding.InputKind ||
		record.EmbeddingInputFingerprint != fingerprints.EmbeddingInput[binding.Name] ||
		space.ID != fingerprints.VectorSpace[binding.Name] {
		return errors.New("embedding set does not match exact processing profile binding identity")
	}
	descriptor := space.Descriptor
	if descriptor.ID != binding.Descriptor.ID || descriptor.Fingerprint != binding.Descriptor.Fingerprint ||
		descriptor.Model != binding.Model || descriptor.Dimension != binding.Dimensions ||
		descriptor.Metric != binding.Metric || descriptor.Normalization != binding.Normalization ||
		descriptor.ScalarEncoding != binding.ScalarEncoding || descriptor.DocumentFormatter != binding.DocumentFormatter ||
		descriptor.QueryFormatter != binding.QueryFormatter || descriptor.CompatibilityID != binding.CompatibilityID ||
		descriptor.ModelInput != binding.ModelInput || string(descriptor.TrustBoundary) != binding.TrustBoundary ||
		!slices.Contains(descriptor.InputKinds, binding.InputKind) {
		return errors.New("E1 descriptor does not match exact processing profile embedding binding")
	}
	if record.InputKind == document.EmbeddingInputRenditionChunk {
		if len(record.InputGeneration.GenerationJSON) == 0 {
			return nil
		}
		var generation document.EmbeddingInputGeneration
		if err := json.Unmarshal(record.InputGeneration.GenerationJSON, &generation); err != nil {
			return err
		}
		if binding.Chunk == nil || generation.ModelInputFingerprint != binding.ModelInput.Fingerprint ||
			generation.LexicalEvidenceFingerprint != fingerprints.EvidenceLexical ||
			generation.Formatter != binding.Chunk.Formatter ||
			generation.TokenizerIdentity.Name+"@"+generation.TokenizerIdentity.Revision != binding.Chunk.Tokenizer ||
			generation.ContentTokenBudget != binding.Chunk.MaxTokens ||
			generation.OverlapTokens != binding.Chunk.OverlapTokens ||
			string(generation.TruncationPolicy) != binding.Chunk.TruncationPolicy ||
			generation.ContextFingerprint != binding.Chunk.ContextFingerprint {
			return errors.New("E2 generation does not match exact processing profile embedding binding")
		}
		for _, input := range generation.Inputs {
			if input.ContentTokens > binding.Chunk.MaxTokens {
				return errors.New("E2 generation exceeds processing profile chunk token bound")
			}
		}
	}
	return nil
}

func insertVectorSpaceTx(ctx context.Context, tx *sql.Tx, record EmbeddingVectorSpaceRecord) error {
	descriptorJSON, err := encodeCanonicalEmbeddingDescriptor(record.Descriptor)
	if err != nil {
		return fmt.Errorf("encoding E1 descriptor: %w", err)
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO embedding_vector_spaces(
		vector_space_id,contract_version,descriptor_json,
		provider_descriptor,provider_revision,
		descriptor_fingerprint,compatibility_id,dimensions,metric,normalization,
		scalar_encoding,document_formatter,query_formatter,model_input_fingerprint
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.ID, record.ContractVersion, descriptorJSON,
		record.ProviderDescriptor, record.ProviderRevision, record.DescriptorFingerprint,
		record.CompatibilityID, record.Dimensions, record.Metric, record.Normalization,
		record.ScalarEncoding, record.DocumentFormatter, record.QueryFormatter,
		record.ModelInputFingerprint)
	if err != nil {
		return fmt.Errorf("inserting embedding vector space: %w", err)
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		stored, err := loadVectorSpaceTx(ctx, tx, record.ID)
		if err != nil || !reflect.DeepEqual(stored, record) {
			return fmt.Errorf("embedding vector space %s names different immutable authority", record.ID)
		}
	}
	return nil
}

func insertInputGenerationTx(ctx context.Context, tx *sql.Tx, record EmbeddingInputGenerationRecord) error {
	var attachment any
	if record.AttachmentID != "" {
		attachment = record.AttachmentID
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO embedding_input_generations(
		generation_id,generation_blob_hash,generation_encoded_size,generation_checksum,source_version_id,profile_fingerprint,
		evidence_fingerprint,tokenizer_fingerprint,chunk_policy_fingerprint,
		formatter_fingerprint,attachment_context_fingerprint,attachment_id,
		input_count,created_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.ID, nullableCatalogString(record.GenerationBlobHash), record.GenerationEncodedSize, record.GenerationChecksum,
		record.SourceVersionID, record.ProcessingProfileFingerprint, record.EvidenceFingerprint,
		record.TokenizerFingerprint, record.ChunkPolicyFingerprint, record.FormatterFingerprint,
		record.AttachmentContextFingerprint, attachment,
		len(record.Inputs), record.CreatedAt)
	if err != nil {
		return fmt.Errorf("inserting embedding input generation: %w", err)
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		stored, err := loadInputGenerationTx(ctx, tx, record.ID)
		if err != nil || !reflect.DeepEqual(stored, record) {
			return fmt.Errorf("embedding input generation %s names different immutable authority", record.ID)
		}
		return nil
	}
	for order, input := range record.Inputs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO embedding_generation_inputs(
			generation_id,input_id,input_order,rendered_checksum) VALUES(?,?,?,?)`,
			record.ID, input.ID, order, input.RenderedChecksum); err != nil {
			return fmt.Errorf("inserting embedding input: %w", err)
		}
	}
	return nil
}

func insertVectorSetTx(ctx context.Context, tx *sql.Tx, record EmbeddingVectorSetRecord) error {
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO embedding_vector_sets(
		vector_set_id,contract_version,vector_space_id,payload_blob_hash,payload_size,payload_checksum,
		manifest_checksum,row_count,dimensions) VALUES(?,?,?,?,?,?,?,?,?)`, record.ID,
		record.ContractVersion, record.VectorSpaceID, record.PayloadBlobHash, record.PayloadSize, record.PayloadChecksum,
		record.ManifestChecksum, record.RowCount, record.Dimensions)
	if err != nil {
		return fmt.Errorf("inserting embedding vector set: %w", err)
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		stored, err := loadVectorSetTx(ctx, tx, record.ID)
		if err != nil || !reflect.DeepEqual(stored, record) {
			return fmt.Errorf("embedding vector set %s names different immutable authority", record.ID)
		}
		return nil
	}
	for order, row := range record.Rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO embedding_vector_rows(
			vector_set_id,row_id,row_order,input_id,dimensions,checksum) VALUES(?,?,?,?,?,?)`,
			record.ID, row.RowID, order, row.InputID, row.Dimensions, row.Checksum); err != nil {
			return fmt.Errorf("inserting embedding vector row: %w", err)
		}
	}
	return nil
}

func loadEmbeddingSetTx(ctx context.Context, tx *sql.Tx, id string) (EmbeddingSetRecord, error) {
	var result EmbeddingSetRecord
	err := tx.QueryRowContext(ctx, `SELECT embedding_set_id,vault_uid,binding_id,input_kind,
		content_version_id,profile_fingerprint,embedding_input_fingerprint,vector_space_id,input_generation_id,
		vector_set_id,created_at FROM embedding_sets WHERE embedding_set_id=?`, id).Scan(
		&result.ID, &result.VaultID, &result.BindingID, &result.InputKind,
		&result.ContentVersionID, &result.ProcessingProfileFingerprint, &result.EmbeddingInputFingerprint, &result.VectorSpace.ID,
		&result.InputGeneration.ID, &result.VectorSet.ID, &result.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EmbeddingSetRecord{}, ErrNotFound
	}
	if err != nil {
		return EmbeddingSetRecord{}, fmt.Errorf("loading embedding set: %w", err)
	}
	if result.VectorSpace, err = loadVectorSpaceTx(ctx, tx, result.VectorSpace.ID); err != nil {
		return EmbeddingSetRecord{}, err
	}
	if result.InputGeneration, err = loadInputGenerationTx(ctx, tx, result.InputGeneration.ID); err != nil {
		return EmbeddingSetRecord{}, err
	}
	if result.VectorSet, err = loadVectorSetTx(ctx, tx, result.VectorSet.ID); err != nil {
		return EmbeddingSetRecord{}, err
	}
	return result, nil
}

func loadVectorSpaceTx(ctx context.Context, tx *sql.Tx, id string) (EmbeddingVectorSpaceRecord, error) {
	var record EmbeddingVectorSpaceRecord
	var descriptorJSON []byte
	err := tx.QueryRowContext(ctx, `SELECT vector_space_id,contract_version,
		descriptor_json,provider_descriptor,provider_revision,descriptor_fingerprint,compatibility_id,
		dimensions,metric,normalization,scalar_encoding,document_formatter,query_formatter,
		model_input_fingerprint FROM embedding_vector_spaces WHERE vector_space_id=?`, id).Scan(
		&record.ID, &record.ContractVersion, &descriptorJSON, &record.ProviderDescriptor, &record.ProviderRevision,
		&record.DescriptorFingerprint, &record.CompatibilityID, &record.Dimensions, &record.Metric,
		&record.Normalization, &record.ScalarEncoding, &record.DocumentFormatter,
		&record.QueryFormatter, &record.ModelInputFingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	if err == nil {
		record.Descriptor, err = decodeCanonicalEmbeddingDescriptor(descriptorJSON)
	}
	return record, err
}

func encodeCanonicalEmbeddingDescriptor(descriptor document.EmbeddingDescriptor) ([]byte, error) {
	canonical, err := document.NewEmbeddingDescriptor(descriptor)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(canonical, descriptor) {
		return nil, errors.New("E1 descriptor is not canonical")
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, err
	}
	if len(encoded) < 2 || len(encoded) > maxEmbeddingDescriptorJSON {
		return nil, errors.New("E1 descriptor JSON exceeds bounds")
	}
	return encoded, nil
}

func decodeCanonicalEmbeddingDescriptor(data []byte) (document.EmbeddingDescriptor, error) {
	if len(data) < 2 || len(data) > maxEmbeddingDescriptorJSON {
		return document.EmbeddingDescriptor{}, errors.New("E1 descriptor JSON exceeds bounds")
	}
	var descriptor document.EmbeddingDescriptor
	if err := jsonv2.Unmarshal(data, &descriptor, jsonv2.RejectUnknownMembers(true)); err != nil {
		return document.EmbeddingDescriptor{}, fmt.Errorf("decoding strict E1 descriptor JSON: %w", err)
	}
	encoded, err := encodeCanonicalEmbeddingDescriptor(descriptor)
	if err != nil {
		return document.EmbeddingDescriptor{}, err
	}
	if !bytes.Equal(encoded, data) {
		return document.EmbeddingDescriptor{}, errors.New("E1 descriptor JSON is not the canonical staged encoding")
	}
	return descriptor, nil
}

func loadInputGenerationTx(ctx context.Context, tx *sql.Tx, id string) (EmbeddingInputGenerationRecord, error) {
	var record EmbeddingInputGenerationRecord
	var attachment sql.NullString
	var count int
	var generationBlob sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT generation_id,generation_blob_hash,generation_encoded_size,generation_checksum,source_version_id,
		profile_fingerprint,evidence_fingerprint,tokenizer_fingerprint,
		chunk_policy_fingerprint,formatter_fingerprint,attachment_context_fingerprint,
		attachment_id,input_count,created_at
		FROM embedding_input_generations WHERE generation_id=?`, id).Scan(&record.ID,
		&generationBlob, &record.GenerationEncodedSize, &record.GenerationChecksum, &record.SourceVersionID, &record.ProcessingProfileFingerprint,
		&record.EvidenceFingerprint, &record.TokenizerFingerprint, &record.ChunkPolicyFingerprint,
		&record.FormatterFingerprint, &record.AttachmentContextFingerprint, &attachment,
		&count, &record.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	if err != nil {
		return record, err
	}
	if attachment.Valid {
		record.AttachmentID = attachment.String
	}
	if generationBlob.Valid {
		record.GenerationBlobHash = generationBlob.String
	}
	rows, err := tx.QueryContext(ctx, `SELECT input_id,rendered_checksum
		FROM embedding_generation_inputs WHERE generation_id=? ORDER BY input_order`, id)
	if err != nil {
		return record, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var input EmbeddingInputReference
		if err := rows.Scan(&input.ID, &input.RenderedChecksum); err != nil {
			return record, err
		}
		record.Inputs = append(record.Inputs, input)
		if len(record.Inputs) > maxEmbeddingCatalogRows {
			return record, errors.New("embedding input generation exceeds bounded count")
		}
	}
	if err := rows.Err(); err != nil {
		return record, err
	}
	if len(record.Inputs) != count {
		return record, errors.New("embedding input generation count is corrupt")
	}
	return record, nil
}

func loadVectorSetTx(ctx context.Context, tx *sql.Tx, id string) (EmbeddingVectorSetRecord, error) {
	var record EmbeddingVectorSetRecord
	err := tx.QueryRowContext(ctx, `SELECT vector_set_id,contract_version,vector_space_id,payload_blob_hash,
		payload_size,payload_checksum,manifest_checksum,row_count,dimensions
		FROM embedding_vector_sets WHERE vector_set_id=?`, id).Scan(&record.ID,
		&record.ContractVersion, &record.VectorSpaceID, &record.PayloadBlobHash, &record.PayloadSize, &record.PayloadChecksum,
		&record.ManifestChecksum, &record.RowCount, &record.Dimensions)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	if err != nil {
		return record, err
	}
	if record.RowCount < 1 || record.RowCount > maxEmbeddingCatalogRows {
		return record, errors.New("embedding vector-set count is corrupt")
	}
	rows, err := tx.QueryContext(ctx, `SELECT row_id,input_id,dimensions,checksum
		FROM embedding_vector_rows WHERE vector_set_id=? ORDER BY row_order`, id)
	if err != nil {
		return record, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var row EmbeddingVectorRowRecord
		if err := rows.Scan(&row.RowID, &row.InputID, &row.Dimensions, &row.Checksum); err != nil {
			return record, err
		}
		record.Rows = append(record.Rows, row)
		if len(record.Rows) > record.RowCount {
			return record, errors.New("embedding vector-set rows exceed declared count")
		}
	}
	if err := rows.Err(); err != nil {
		return record, err
	}
	if len(record.Rows) != record.RowCount || embeddingVectorManifestChecksum(record.Rows) != record.ManifestChecksum {
		return record, errors.New("embedding vector-set manifest is corrupt")
	}
	return record, nil
}

func embeddingSetEligibleTx(ctx context.Context, tx *sql.Tx, set EmbeddingSetRecord) (bool, error) {
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM content_versions v
		JOIN nodes n ON n.id=v.node_id AND n.current_version_id=v.version_id AND n.trashed_at IS NULL
		WHERE v.version_id=?`, set.ContentVersionID).Scan(&live); err != nil {
		return false, err
	}
	if live != 1 {
		return false, nil
	}
	if set.InputGeneration.AttachmentID == "" {
		return true, nil
	}
	var attached int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rendition_heads h
		JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
		WHERE h.content_version_id=? AND h.profile_fingerprint=? AND h.attachment_id=?`,
		set.ContentVersionID, set.ProcessingProfileFingerprint,
		set.InputGeneration.AttachmentID).Scan(&attached); err != nil {
		return false, err
	}
	return attached == 1, nil
}

func validateEmbeddingHeadKey(key EmbeddingHeadKey) error {
	if err := validateUUIDv4(key.ContentVersionID); err != nil {
		return fmt.Errorf("embedding head content version: %w", err)
	}
	if err := validateEmbeddingCatalogText(key.BindingID, "embedding head binding"); err != nil {
		return err
	}
	return validateEmbeddingInputKind(key.InputKind)
}

func validateEmbeddingHeadRecord(record EmbeddingHeadRecord) error {
	if err := validateEmbeddingHeadKey(record.Key); err != nil {
		return err
	}
	for value, subject := range map[string]string{
		record.SetID: "embedding head set ID", record.VectorSpaceID: "embedding head vector-space ID",
		record.ProcessingProfileFingerprint: "embedding head profile fingerprint",
	} {
		if err := validateCatalogSHA256(value, subject); err != nil {
			return err
		}
	}
	return validateMetadataTime("embedding head published_at", record.PublishedAt)
}

func validateEmbeddingFailureRecord(record EmbeddingFailureRecord) error {
	if err := validateEmbeddingHeadKey(EmbeddingHeadKey{record.ContentVersionID, record.BindingID, record.InputKind}); err != nil {
		return err
	}
	if err := validateCatalogSHA256(record.ProcessingProfileFingerprint, "embedding failure profile"); err != nil {
		return err
	}
	switch record.FailureCode {
	case EmbeddingFailureProviderUnavailable, EmbeddingFailureAuthorization,
		EmbeddingFailureInvalidResponse, EmbeddingFailureInputRejected, EmbeddingFailureStaleAuthority:
	default:
		return errors.New("embedding failure code is not in the provider-neutral vocabulary")
	}
	return validateMetadataTime("embedding failure failed_at", record.FailedAt)
}

func validateCoverageScope(scope CoverageScope) error {
	if len(scope.ContentVersionIDs) < 1 || len(scope.ContentVersionIDs) > maxEmbeddingCorpusMembers ||
		len(scope.Bindings) < 1 || len(scope.Bindings) > maxEmbeddingCatalogRows {
		return errors.New("embedding coverage scope is outside bounded counts")
	}
	if len(scope.ContentVersionIDs) > maxEmbeddingCoverageWork/len(scope.Bindings) {
		return errors.New("embedding coverage work exceeds bounded cells")
	}
	if err := validateCatalogSHA256(scope.ProcessingProfileFingerprint, "coverage profile fingerprint"); err != nil {
		return err
	}
	seenVersions := make(map[string]struct{}, len(scope.ContentVersionIDs))
	for _, versionID := range scope.ContentVersionIDs {
		if err := validateUUIDv4(versionID); err != nil {
			return fmt.Errorf("coverage content version: %w", err)
		}
		if _, duplicate := seenVersions[versionID]; duplicate {
			return errors.New("coverage scope contains duplicate content versions")
		}
		seenVersions[versionID] = struct{}{}
	}
	seenBindings := make(map[string]struct{}, len(scope.Bindings))
	for _, binding := range scope.Bindings {
		if err := validateEmbeddingCatalogText(binding.BindingID, "coverage binding ID"); err != nil {
			return err
		}
		if err := validateEmbeddingInputKind(binding.InputKind); err != nil {
			return err
		}
		if binding.VectorSpaceID != "" {
			if err := validateCatalogSHA256(binding.VectorSpaceID, "coverage vector-space ID"); err != nil {
				return err
			}
		}
		identity := binding.BindingID + "\x00" + string(binding.InputKind)
		if _, duplicate := seenBindings[identity]; duplicate {
			return errors.New("coverage scope contains duplicate bindings")
		}
		seenBindings[identity] = struct{}{}
	}
	return nil
}

func summarizeBindingCoverage(item BindingCoverage) CoverageState {
	if item.Ineligible == item.Total {
		return CoverageIneligible
	}
	if item.Rebuilding > 0 {
		return CoverageRebuilding
	}
	if item.Complete == item.Total {
		return CoverageComplete
	}
	if item.Complete > 0 {
		return CoveragePartial
	}
	if item.Stale > 0 {
		return CoverageStale
	}
	return CoverageUnavailable
}

func summarizeCoverage(coverage Coverage) CoverageState {
	for _, item := range append(slices.Clone(coverage.Required), coverage.Optional...) {
		if item.State == CoverageRebuilding {
			return CoverageRebuilding
		}
	}
	if len(coverage.Required) == 0 {
		for _, item := range coverage.Optional {
			if item.State != CoverageComplete {
				return CoveragePartial
			}
		}
		return CoverageComplete
	}
	allIneligible := true
	allComplete := true
	for _, item := range coverage.Required {
		if item.State != CoverageIneligible {
			allIneligible = false
		}
		if item.State != CoverageComplete {
			allComplete = false
		}
	}
	if allIneligible {
		return CoverageIneligible
	}
	if allComplete {
		return CoverageComplete
	}
	return CoveragePartial
}

func validateEmbeddingInputKind(kind EmbeddingInputKind) error {
	switch kind {
	case document.EmbeddingInputRenditionChunk, document.EmbeddingInputOriginalFile:
		return nil
	default:
		return fmt.Errorf("embedding input kind %q is unsupported", kind)
	}
}

func validateEmbeddingCatalogText(value, subject string) error {
	if strings.TrimSpace(value) == "" || len(value) > maxEmbeddingCatalogIDBytes {
		return fmt.Errorf("%s must be bounded non-empty text", subject)
	}
	return nil
}

type embeddingCatalogTableSchema struct {
	name    string
	columns []string
}

var embeddingCatalogSchema = []embeddingCatalogTableSchema{
	{"embedding_vector_spaces", []string{metadataEmbeddingVectorSpaceIDField, "contract_version", "descriptor_json", "provider_descriptor", "provider_revision", "descriptor_fingerprint", "compatibility_id", "dimensions", "metric", "normalization", "scalar_encoding", "document_formatter", "query_formatter", "model_input_fingerprint"}},
	{"embedding_input_generations", []string{"generation_id", "generation_blob_hash", "generation_encoded_size", "generation_checksum", auditSourceVersionIDField, metadataEmbeddingProfileField, "evidence_fingerprint", "tokenizer_fingerprint", "chunk_policy_fingerprint", "formatter_fingerprint", "attachment_context_fingerprint", "attachment_id", "input_count", "created_at"}},
	{"embedding_generation_inputs", []string{"generation_id", "input_id", "input_order", "rendered_checksum"}},
	{"embedding_vector_sets", []string{"vector_set_id", "contract_version", metadataEmbeddingVectorSpaceIDField, "payload_blob_hash", "payload_size", "payload_checksum", "manifest_checksum", "row_count", "dimensions"}},
	{"embedding_vector_rows", []string{"vector_set_id", "row_id", "row_order", "input_id", "dimensions", "checksum"}},
	{"embedding_sets", []string{"embedding_set_id", "vault_uid", "binding_id", "input_kind", "content_version_id", metadataEmbeddingProfileField, "embedding_input_fingerprint", metadataEmbeddingVectorSpaceIDField, "input_generation_id", "vector_set_id", "created_at"}},
	{"embedding_heads", []string{"content_version_id", "binding_id", "input_kind", "embedding_set_id", metadataEmbeddingVectorSpaceIDField, metadataEmbeddingProfileField, "published_at"}},
	{"embedding_failures", []string{"content_version_id", metadataEmbeddingProfileField, "binding_id", "input_kind", "failure_code", "failed_at"}},
	{"embedding_corpus_generations", []string{"corpus_generation_id", "contract_version", "binding_id", metadataEmbeddingVectorSpaceIDField, "manifest_checksum", "member_count", "created_at"}},
	{"embedding_corpus_members", []string{"corpus_generation_id", "member_order", "embedding_set_id"}},
}

func validateEmbeddingCatalogSchemaTx(ctx context.Context, tx *sql.Tx) error {
	for _, table := range embeddingCatalogSchema {
		got, err := embeddingCatalogSchemaColumnsTx(ctx, tx, table.name)
		if err != nil {
			return fmt.Errorf("validating embedding catalog schema %s: %w", table.name, err)
		}
		if !slices.Equal(got, table.columns) {
			return fmt.Errorf("embedding catalog schema %s columns are invalid", table.name)
		}
	}
	return nil
}

func embeddingCatalogSchemaColumnsTx(ctx context.Context, tx *sql.Tx, table string) (_ []string, retErr error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}

// sortedCorpusSetIDs returns deterministic logical ordering for corpus
// manifests and is shared by staging and metadata validation.
func sortedCorpusSetIDs(values []string) ([]string, error) {
	if len(values) > maxEmbeddingCorpusMembers {
		return nil, errors.New("embedding corpus membership exceeds bounds")
	}
	result := append([]string(nil), values...)
	for _, value := range result {
		if err := validateCatalogSHA256(value, "embedding corpus set ID"); err != nil {
			return nil, err
		}
	}
	sort.Strings(result)
	for index := 1; index < len(result); index++ {
		if result[index] == result[index-1] {
			return nil, errors.New("embedding corpus contains duplicate sets")
		}
	}
	return result, nil
}
